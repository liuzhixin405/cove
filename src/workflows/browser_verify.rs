//! Browser verification: a declarative workflow replayed at desktop (1280x800)
//! and mobile (390x844) viewports, with a JSON report (pass / fail /
//! unverified) and PNG evidence under `<data>/browser-verification/<run-id>/`.
//!
//! A false assertion or a missing element is `fail`. Safety refusals, network
//! failures, cancellation, invalid workflows and a missing browser are
//! `unverified`, never a false green. The report records no fixture values,
//! selectors, page text or URLs; screenshots may still show page content.

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    time::Duration,
};

use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    session::{atomic_json, new_id, now, read_json},
    tools::browser::{self, Browser, Page, Reach},
    workflows::changes::sha256_hex,
};

const MAX_WORKFLOW: u64 = 64 * 1024;
const MAX_STEPS: usize = 32;

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Workflow {
    pub url: String,
    #[serde(default)]
    pub timeout_ms: Option<u64>,
    #[serde(default)]
    pub step_timeout_ms: Option<u64>,
    #[serde(default)]
    pub fixtures: BTreeMap<String, String>,
    pub steps: Vec<Step>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Step {
    pub action: Action,
    #[serde(default)]
    pub selector: Option<String>,
    #[serde(default)]
    pub url: Option<String>,
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub fixture: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Action {
    Navigate,
    Click,
    Fill,
    AssertText,
    AssertVisible,
    AssertUrl,
}

impl Workflow {
    pub fn read(path: &Path) -> Result<Self> {
        if std::fs::metadata(path)?.len() > MAX_WORKFLOW {
            return Err(Error::Tool("workflow exceeds 64 KiB".into()));
        }
        let bytes = std::fs::read(path)?;
        let bytes = bytes.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&bytes);
        let workflow: Self = serde_json::from_slice(bytes)
            .map_err(|err| Error::Tool(format!("invalid workflow: {err}")))?;
        workflow.validate()?;
        Ok(workflow)
    }

    pub fn validate(&self) -> Result<()> {
        let http = |url: &str| {
            url::Url::parse(url)
                .ok()
                .is_some_and(|url| matches!(url.scheme(), "http" | "https"))
        };
        if !http(&self.url) {
            return Err(Error::Tool("workflow url must be http(s)".into()));
        }
        if self.steps.is_empty() || self.steps.len() > MAX_STEPS {
            return Err(Error::Tool(format!("workflow needs 1..{MAX_STEPS} steps")));
        }
        if !self.steps.iter().any(|step| {
            matches!(
                step.action,
                Action::AssertText | Action::AssertVisible | Action::AssertUrl
            )
        }) {
            return Err(Error::Tool(
                "workflow needs at least one explicit assertion".into(),
            ));
        }
        for (index, step) in self.steps.iter().enumerate() {
            let fail = |what: &str| Err(Error::Tool(format!("step {}: {what}", index + 1)));
            let selector = step
                .selector
                .as_deref()
                .is_some_and(|s| !s.trim().is_empty() && s.len() <= 1024);
            match step.action {
                Action::Navigate | Action::AssertUrl => {
                    if !step.url.as_deref().is_some_and(http) {
                        return fail("needs an http(s) url");
                    }
                }
                Action::Click | Action::AssertVisible if !selector => {
                    return fail("needs a selector");
                }
                Action::Fill => {
                    if !selector {
                        return fail("needs a selector");
                    }
                    match (&step.text, &step.fixture) {
                        (Some(_), None) => {}
                        (None, Some(name)) if self.fixtures.contains_key(name) => {}
                        _ => return fail("needs text or a known fixture"),
                    }
                }
                Action::AssertText
                    if !selector || step.text.as_deref().is_none_or(str::is_empty) =>
                {
                    return fail("needs a selector and text");
                }
                _ => {}
            }
        }
        if self
            .timeout_ms
            .is_some_and(|ms| !(1000..=600_000).contains(&ms))
            || self
                .step_timeout_ms
                .is_some_and(|ms| !(100..=60_000).contains(&ms))
        {
            return Err(Error::Tool("timeouts out of range".into()));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    Pass,
    Fail,
    Unverified,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct StepEvidence {
    pub index: usize,
    pub action: Action,
    pub status: Status,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Artifact {
    pub name: String,
    pub sha256: String,
    pub bytes: usize,
    pub width: u32,
    pub height: u32,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ViewportReport {
    pub name: String,
    pub width: u32,
    pub height: u32,
    pub status: Status,
    #[serde(default)]
    pub reason: Option<String>,
    pub steps: Vec<StepEvidence>,
    #[serde(default)]
    pub blocked_requests: usize,
    #[serde(default)]
    pub artifact: Option<Artifact>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Report {
    pub version: u32,
    pub id: String,
    pub project: PathBuf,
    pub status: Status,
    #[serde(default)]
    pub reason: Option<String>,
    pub started: u64,
    pub duration_ms: u64,
    pub allow_local: bool,
    pub viewports: Vec<ViewportReport>,
}

pub fn directory(data_dir: &Path) -> PathBuf {
    data_dir.join("browser-verification")
}

const VIEWPORTS: [(&str, u32, u32, bool); 2] =
    [("desktop", 1280, 800, false), ("mobile", 390, 844, true)];

fn literal(text: &str) -> String {
    serde_json::to_string(text).unwrap_or_else(|_| "\"\"".into())
}

/// Outcome of one step: `Err((status, reason))` stops the viewport.
async fn step(
    page: &Page<'_>,
    workflow: &Workflow,
    step: &Step,
    timeout: Duration,
) -> std::result::Result<(), (Status, String)> {
    let unverified = |err: Error| match err {
        Error::Approval(reason) => (Status::Unverified, format!("safety: {reason}")),
        Error::Cancelled => (Status::Unverified, "cancelled".into()),
        other => (Status::Unverified, other.to_string()),
    };
    let selector = step.selector.as_deref().map(literal).unwrap_or_default();
    // Polls until the element exists (or the step times out).
    let find = |check: String| {
        let selector = selector.clone();
        async move {
            let deadline = tokio::time::Instant::now() + timeout;
            loop {
                let value = page
                    .eval(&format!(
                        "(() => {{ const el = document.querySelector({selector}); if (!el) return null; {check} }})()"
                    ))
                    .await
                    .map_err(unverified)?;
                if !value.is_null() {
                    return Ok(value);
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err((Status::Fail, "element not found in time".to_owned()));
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
        }
    };
    match step.action {
        Action::Navigate => page
            .navigate(
                step.url.as_deref().unwrap_or(""),
                timeout.max(Duration::from_secs(10)),
            )
            .await
            .map_err(unverified),
        Action::Click => {
            find("el.scrollIntoView({block:'center'}); el.click(); return true;".into()).await?;
            // Give a click-triggered navigation or render a moment, then settle.
            tokio::time::sleep(Duration::from_millis(200)).await;
            page.settle(timeout.max(Duration::from_secs(5)))
                .await
                .map_err(unverified)
        }
        Action::Fill => {
            let value = match (&step.text, &step.fixture) {
                (Some(text), _) => text.clone(),
                (None, Some(name)) => workflow.fixtures.get(name).cloned().unwrap_or_default(),
                _ => String::new(),
            };
            let result = find(format!(
                "if ((el.type || '').toLowerCase() === 'password') return 'password'; el.focus(); \
                 const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype; \
                 const setter = Object.getOwnPropertyDescriptor(proto, 'value'); \
                 if (setter && setter.set && (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement)) setter.set.call(el, {v}); else el.value = {v}; \
                 el.dispatchEvent(new Event('input', {{bubbles:true}})); el.dispatchEvent(new Event('change', {{bubbles:true}})); return true;",
                v = literal(&value)
            ))
            .await?;
            if result == json!("password") {
                return Err((
                    Status::Unverified,
                    "password fields are not filled by verification workflows".into(),
                ));
            }
            Ok(())
        }
        Action::AssertVisible => {
            let visible = find(
                "const r = el.getBoundingClientRect(); const s = getComputedStyle(el); \
                 return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none' && Number(s.opacity) > 0;"
                    .into(),
            )
            .await?;
            if visible == json!(true) {
                Ok(())
            } else {
                Err((Status::Fail, "element is not visible".into()))
            }
        }
        Action::AssertText => {
            let text = step.text.as_deref().unwrap_or("");
            let deadline = tokio::time::Instant::now() + timeout;
            loop {
                let found = find(format!(
                    "return ((el.innerText || el.textContent || '')).includes({});",
                    literal(text)
                ))
                .await?;
                if found == json!(true) {
                    return Ok(());
                }
                if tokio::time::Instant::now() >= deadline {
                    return Err((Status::Fail, "expected text not present".into()));
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
        }
        Action::AssertUrl => {
            let expected = step.url.as_deref().unwrap_or("");
            let actual = page.eval("location.href").await.map_err(unverified)?;
            let actual = actual.as_str().unwrap_or("");
            let same = |a: &str, b: &str| a.trim_end_matches('/') == b.trim_end_matches('/');
            if same(actual, expected) {
                Ok(())
            } else {
                Err((
                    Status::Fail,
                    "location does not match the expected URL".into(),
                ))
            }
        }
    }
}

async fn viewport(
    browser: &Browser,
    workflow: &Workflow,
    (name, width, height, mobile): (&str, u32, u32, bool),
    evidence: &Path,
    cancel: &CancellationToken,
) -> ViewportReport {
    browser.guard.reset();
    let mut report = ViewportReport {
        name: name.into(),
        width,
        height,
        status: Status::Pass,
        reason: None,
        steps: Vec::new(),
        blocked_requests: 0,
        artifact: None,
    };
    let page = match browser.page(width, height, mobile).await {
        Ok(page) => page,
        Err(err) => {
            report.status = Status::Unverified;
            report.reason = Some(err.to_string());
            return report;
        }
    };
    let timeout = Duration::from_millis(workflow.step_timeout_ms.unwrap_or(10_000));
    let start = Step {
        action: Action::Navigate,
        selector: None,
        url: Some(workflow.url.clone()),
        text: None,
        fixture: None,
    };
    for (index, current) in std::iter::once(&start).chain(&workflow.steps).enumerate() {
        let outcome = tokio::select! {
            _ = cancel.cancelled() => Err((Status::Unverified, "cancelled".to_owned())),
            outcome = step(&page, workflow, current, timeout) => outcome,
        };
        let status = match &outcome {
            Ok(()) => Status::Pass,
            Err((status, _)) => *status,
        };
        report.steps.push(StepEvidence {
            index,
            action: current.action,
            status,
        });
        if let Err((status, reason)) = outcome {
            report.status = status;
            report.reason = Some(reason);
            break;
        }
    }
    report.blocked_requests = browser.guard.blocked();
    if report.status == Status::Pass && report.blocked_requests > 0 {
        report.status = Status::Unverified;
        report.reason = Some(if browser.guard.network_failed() {
            "network_unavailable: a subresource host did not resolve".into()
        } else {
            "the request guard refused page requests".into()
        });
    }
    if !cancel.is_cancelled()
        && let Ok(png) = page.screenshot(4000).await
        && png.len() <= 4 * 1024 * 1024
    {
        let file = format!("{name}.png");
        if std::fs::write(evidence.join(&file), &png).is_ok() {
            let (width, height) = png_size(&png).unwrap_or((0, 0));
            report.artifact = Some(Artifact {
                name: file,
                sha256: sha256_hex(&png),
                bytes: png.len(),
                width,
                height,
            });
        }
    }
    page.close().await;
    report
}

pub fn png_size(png: &[u8]) -> Option<(u32, u32)> {
    if png.len() < 24 || !png.starts_with(crate::tools::draw::PNG_SIGNATURE) {
        return None;
    }
    let read = |at: usize| u32::from_be_bytes([png[at], png[at + 1], png[at + 2], png[at + 3]]);
    Some((read(16), read(20)))
}

/// Runs `workflow` and stores the report; never errors for page outcomes.
pub async fn run(
    data_dir: &Path,
    project: &Path,
    workflow_path: &Path,
    allow_local: bool,
    browser_path: Option<&Path>,
    cancel: &CancellationToken,
) -> Result<Report> {
    let started = std::time::Instant::now();
    let id = new_id();
    let evidence = directory(data_dir).join(&id);
    std::fs::create_dir_all(&evidence)?;
    let mut report = Report {
        version: 1,
        id,
        project: project.to_path_buf(),
        status: Status::Unverified,
        reason: None,
        started: now(),
        duration_ms: 0,
        allow_local,
        viewports: Vec::new(),
    };
    let workflow = match Workflow::read(workflow_path) {
        Ok(workflow) => Some(workflow),
        Err(err) => {
            report.reason = Some(err.to_string());
            None
        }
    };
    let reach = if allow_local {
        Reach::PublicAndLoopback
    } else {
        Reach::PublicOnly
    };
    if let Some(workflow) = workflow {
        match browser::detect(browser_path) {
            None => {
                report.reason =
                    Some("no Chrome/Edge found (set browser_path or COVE_BROWSER)".into())
            }
            Some(executable) => {
                let total = Duration::from_millis(workflow.timeout_ms.unwrap_or(120_000));
                let run = async {
                    let browser = Browser::launch(&executable, reach, cancel).await?;
                    let mut viewports = Vec::new();
                    for spec in VIEWPORTS {
                        viewports
                            .push(viewport(&browser, &workflow, spec, &evidence, cancel).await);
                    }
                    browser.close().await;
                    Ok::<_, Error>(viewports)
                };
                match tokio::time::timeout(total, run).await {
                    Ok(Ok(viewports)) => {
                        report.status = if viewports.iter().any(|v| v.status == Status::Fail) {
                            Status::Fail
                        } else if viewports.iter().all(|v| v.status == Status::Pass) {
                            Status::Pass
                        } else {
                            Status::Unverified
                        };
                        report.viewports = viewports;
                    }
                    Ok(Err(err)) => report.reason = Some(err.to_string()),
                    Err(_) => report.reason = Some("workflow timeout".into()),
                }
            }
        }
    }
    report.duration_ms = started.elapsed().as_millis() as u64;
    atomic_json(&evidence.join("report.json"), &report)?;
    Ok(report)
}

pub fn list(data_dir: &Path, project: &Path) -> Vec<Report> {
    let mut reports: Vec<Report> = std::fs::read_dir(directory(data_dir))
        .into_iter()
        .flatten()
        .flatten()
        .filter_map(|entry| read_json::<Report>(&entry.path().join("report.json")).ok())
        .filter(|report| report.project == project)
        .collect();
    reports.sort_by_key(|report| std::cmp::Reverse(report.started));
    reports.truncate(20);
    reports
}

pub fn render(report: &Report) -> String {
    let mut text = format!(
        "browser verification {} {:?}{} in {:.1}s\n",
        report.id,
        report.status,
        report
            .reason
            .as_ref()
            .map_or(String::new(), |reason| format!(" ({reason})")),
        report.duration_ms as f64 / 1000.0
    );
    for viewport in &report.viewports {
        text.push_str(&format!(
            "  {} {}x{}: {:?}{} steps={} blocked={}{}\n",
            viewport.name,
            viewport.width,
            viewport.height,
            viewport.status,
            viewport
                .reason
                .as_ref()
                .map_or(String::new(), |reason| format!(" ({reason})")),
            viewport.steps.len(),
            viewport.blocked_requests,
            viewport
                .artifact
                .as_ref()
                .map_or(String::new(), |a| format!(
                    " evidence={} {}x{}",
                    a.name, a.width, a.height
                ))
        ));
    }
    text
}

#[cfg(test)]
mod tests {
    use super::*;

    fn workflow(value: serde_json::Value) -> Result<Workflow> {
        let workflow: Workflow =
            serde_json::from_value(value).map_err(|err| Error::Tool(err.to_string()))?;
        workflow.validate()?;
        Ok(workflow)
    }

    #[test]
    fn workflows_need_assertions_and_valid_steps() {
        assert!(
            workflow(
                json!({"url":"http://127.0.0.1:3000","fixtures":{"r":"x"},"steps":[
            {"action":"fill","selector":"#n","fixture":"r"},{"action":"click","selector":"#s"},
            {"action":"assert_text","selector":"#out","text":"ok"}]})
            )
            .is_ok()
        );
        assert!(
            workflow(json!({"url":"http://x.test","steps":[{"action":"click","selector":"#a"}]}))
                .is_err()
        );
        assert!(
            workflow(json!({"url":"file:///x","steps":[{"action":"assert_url","url":"http://x"}]}))
                .is_err()
        );
        assert!(workflow(json!({"url":"http://x.test","steps":[{"action":"fill","selector":"#a","fixture":"missing"},{"action":"assert_visible","selector":"#a"}]})).is_err());
        assert!(
            workflow(json!({"url":"http://x.test","steps":[{"action":"eval","selector":"#a"}]}))
                .is_err()
        );
    }

    #[tokio::test]
    async fn invalid_workflow_or_missing_browser_is_unverified_not_pass() {
        let data = tempfile::tempdir().unwrap();
        let file = data.path().join("w.json");
        std::fs::write(&file, "{}").unwrap();
        let report = run(
            data.path(),
            data.path(),
            &file,
            false,
            None,
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(report.status, Status::Unverified);
        assert_eq!(list(data.path(), data.path()).len(), 1);
    }

    #[test]
    fn png_dimensions_are_read_from_the_header() {
        let canvas = crate::tools::draw::Canvas::new(7, 3).unwrap();
        assert_eq!(png_size(&canvas.png().unwrap()), Some((7, 3)));
    }
}
