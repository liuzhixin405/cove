//! The interactive `question` tool: several questions, each with optional numbered options.

use serde::Deserialize;

use crate::domain::{Error, Result};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct QuestionArgs {
    questions: Vec<Question>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Question {
    question: String,
    #[serde(default)]
    header: Option<String>,
    #[serde(default)]
    options: Vec<Choice>,
    #[serde(default)]
    multiple: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Choice {
    label: String,
    #[serde(default)]
    description: Option<String>,
}

impl QuestionArgs {
    pub(super) fn validate(&self) -> Result<()> {
        if self.questions.is_empty() || self.questions.len() > 8 {
            return Err(Error::Tool("ask 1..8 questions".into()));
        }
        for question in &self.questions {
            if question.question.trim().is_empty() || question.question.len() > 4096 {
                return Err(Error::Tool(
                    "each question must be nonempty and <= 4096 bytes".into(),
                ));
            }
            if question.options.len() > 12
                || question
                    .options
                    .iter()
                    .any(|choice| choice.label.trim().is_empty() || choice.label.len() > 200)
            {
                return Err(Error::Tool(
                    "at most 12 options per question, each with a label of 1..200 bytes".into(),
                ));
            }
        }
        Ok(())
    }

    pub(super) fn len(&self) -> usize {
        self.questions.len()
    }

    /// The terminal prompt for question `index`.
    pub(super) fn prompt(&self, index: usize) -> String {
        let question = &self.questions[index];
        let mut out = format!("[Q{}/{}]", index + 1, self.questions.len());
        if let Some(header) = question.header.as_deref().filter(|h| !h.trim().is_empty()) {
            out.push_str(&format!(" {}", header.trim()));
        }
        out.push_str(&format!("\n{}\n", question.question.trim()));
        for (number, choice) in question.options.iter().enumerate() {
            out.push_str(&format!("  {}. {}", number + 1, choice.label.trim()));
            if let Some(description) = choice
                .description
                .as_deref()
                .filter(|d| !d.trim().is_empty())
            {
                out.push_str(&format!(" - {}", description.trim()));
            }
            out.push('\n');
        }
        if !question.options.is_empty() {
            out.push_str(if question.multiple {
                "输入编号（可多选，如 1,3），或直接输入你的回答\n"
            } else {
                "Answer with a number or your own text\n"
            });
        }
        out.push_str("> ");
        out
    }

    /// Whether the user answered a question that offers options with words of their
    /// own instead of choosing: such a reply can change what the remaining questions
    /// should be, so they are not asked.
    pub(super) fn own_words(&self, index: usize, raw: &str) -> bool {
        let question = &self.questions[index];
        let raw = raw.trim();
        !question.options.is_empty()
            && !raw.is_empty()
            && !raw
                .split([',', ' '])
                .filter(|part| !part.is_empty())
                .all(|part| {
                    part.parse::<usize>()
                        .is_ok_and(|n| (1..=question.options.len()).contains(&n))
                })
    }

    /// Maps option numbers to labels; anything else is kept as free text.
    pub(super) fn answer(&self, index: usize, raw: &str) -> String {
        let question = &self.questions[index];
        let raw = raw.trim();
        let numbers: Option<Vec<usize>> = raw
            .split([',', ' '])
            .filter(|part| !part.is_empty())
            .map(|part| part.parse::<usize>().ok())
            .collect();
        let selected = match numbers {
            Some(numbers)
                if !numbers.is_empty()
                    && (question.multiple || numbers.len() == 1)
                    && numbers
                        .iter()
                        .all(|n| (1..=question.options.len()).contains(n)) =>
            {
                numbers
                    .iter()
                    .map(|n| question.options[n - 1].label.trim())
                    .collect::<Vec<_>>()
                    .join(", ")
            }
            _ if raw.is_empty() => "(no answer)".into(),
            _ => raw.to_owned(),
        };
        format!(
            "[Q{}] {}\nAnswer: {selected}",
            index + 1,
            question.question.trim()
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn numbered_options_map_to_labels_and_free_text_is_kept() {
        let args: QuestionArgs = serde_json::from_value(json!({"questions":[
            {"question":"Which DB?","header":"Storage","options":[{"label":"Postgres","description":"server"},{"label":"SQLite"}]},
            {"question":"Features?","options":[{"label":"a"},{"label":"b"},{"label":"c"}],"multiple":true}
        ]}))
        .unwrap();
        args.validate().unwrap();
        let prompt = args.prompt(0);
        assert!(
            prompt.starts_with("[Q1/2] Storage\nWhich DB?\n  1. Postgres - server\n  2. SQLite\n"),
            "{prompt}"
        );
        assert_eq!(args.answer(0, "2"), "[Q1] Which DB?\nAnswer: SQLite");
        assert!(!args.own_words(0, "2") && !args.own_words(0, " "));
        assert!(args.own_words(0, "first get Jira access working"));
        assert!(args.own_words(0, "9"));
        assert_eq!(args.answer(0, "1,2"), "[Q1] Which DB?\nAnswer: 1,2");
        assert_eq!(args.answer(1, "1, 3"), "[Q2] Features?\nAnswer: a, c");
        assert_eq!(
            args.answer(1, "use redis"),
            "[Q2] Features?\nAnswer: use redis"
        );
    }
}
