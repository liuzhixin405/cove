//! AWS event-stream framing (`application/vnd.amazon.eventstream`), as used by
//! Bedrock Runtime `InvokeModelWithResponseStream`.
//!
//! Frame: total length (u32 BE), headers length (u32 BE), prelude CRC (u32), headers,
//! payload, message CRC (u32). TLS already protects integrity, so CRCs are not checked;
//! lengths are, so a malformed frame cannot make the decoder read past its buffer.

use base64::Engine;

use crate::domain::{Error, Result};

const PRELUDE: usize = 12;
const MAX_FRAME: usize = 16 * 1024 * 1024;

#[derive(Debug, PartialEq, Eq)]
pub struct Frame {
    pub message_type: String,
    pub event_type: String,
    pub exception_type: String,
    pub payload: Vec<u8>,
}

#[derive(Default)]
pub struct Decoder {
    buffer: Vec<u8>,
}

fn be32(bytes: &[u8]) -> usize {
    u32::from_be_bytes([bytes[0], bytes[1], bytes[2], bytes[3]]) as usize
}

impl Decoder {
    /// Frames completed by `chunk`.
    pub fn push(&mut self, chunk: &[u8]) -> Result<Vec<Frame>> {
        self.buffer.extend_from_slice(chunk);
        let mut frames = Vec::new();
        loop {
            if self.buffer.len() < PRELUDE {
                return Ok(frames);
            }
            let total = be32(&self.buffer[0..4]);
            let headers = be32(&self.buffer[4..8]);
            if !(PRELUDE + 4..=MAX_FRAME).contains(&total) || PRELUDE + headers + 4 > total {
                return Err(Error::Provider(
                    "malformed Bedrock event-stream frame".into(),
                ));
            }
            if self.buffer.len() < total {
                return Ok(frames);
            }
            let frame: Vec<u8> = self.buffer.drain(..total).collect();
            let mut parsed = Frame {
                message_type: String::new(),
                event_type: String::new(),
                exception_type: String::new(),
                payload: frame[PRELUDE + headers..total - 4].to_vec(),
            };
            let mut at = PRELUDE;
            let end = PRELUDE + headers;
            while at < end {
                let name_len = frame[at] as usize;
                at += 1;
                if at + name_len + 1 > end {
                    return Err(Error::Provider("malformed event-stream header".into()));
                }
                let name = String::from_utf8_lossy(&frame[at..at + name_len]).into_owned();
                at += name_len;
                let kind = frame[at];
                at += 1;
                // Only string values (type 7) matter here; skip the others by their size.
                let size = match kind {
                    0 | 1 => 0,
                    2 => 1,
                    3 => 2,
                    4 => 4,
                    5 | 8 => 8,
                    9 => 16,
                    6 | 7 => {
                        if at + 2 > end {
                            return Err(Error::Provider("malformed event-stream header".into()));
                        }
                        let len = u16::from_be_bytes([frame[at], frame[at + 1]]) as usize;
                        at += 2;
                        len
                    }
                    _ => return Err(Error::Provider("unknown event-stream header type".into())),
                };
                if at + size > end {
                    return Err(Error::Provider("malformed event-stream header".into()));
                }
                if kind == 7 {
                    let value = String::from_utf8_lossy(&frame[at..at + size]).into_owned();
                    match name.as_str() {
                        ":message-type" => parsed.message_type = value,
                        ":event-type" => parsed.event_type = value,
                        ":exception-type" => parsed.exception_type = value,
                        _ => {}
                    }
                }
                at += size;
            }
            frames.push(parsed);
        }
    }
}

/// A Bedrock `chunk` event carries `{"bytes": base64(<provider stream event JSON>)}`.
pub fn chunk_json(frame: &Frame) -> Result<Option<String>> {
    if frame.message_type == "exception" || frame.message_type == "error" {
        let detail = serde_json::from_slice::<serde_json::Value>(&frame.payload)
            .ok()
            .and_then(|value| {
                value
                    .get("message")
                    .and_then(|m| m.as_str())
                    .map(str::to_owned)
            })
            .unwrap_or_else(|| String::from_utf8_lossy(&frame.payload).into_owned());
        let kind = if frame.exception_type.is_empty() {
            "exception"
        } else {
            frame.exception_type.as_str()
        };
        return Err(Error::Provider(format!("Bedrock {kind}: {detail}")));
    }
    if frame.event_type != "chunk" {
        return Ok(None);
    }
    let value: serde_json::Value = serde_json::from_slice(&frame.payload)?;
    let Some(encoded) = value.get("bytes").and_then(|bytes| bytes.as_str()) else {
        return Ok(None);
    };
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .map_err(|_| Error::Provider("Bedrock chunk is not valid base64".into()))?;
    Ok(Some(String::from_utf8(bytes).map_err(|_| {
        Error::Provider("Bedrock chunk is not UTF-8".into())
    })?))
}

#[cfg(test)]
pub(crate) fn encode(headers: &[(&str, &str)], payload: &[u8]) -> Vec<u8> {
    let mut head = Vec::new();
    for (name, value) in headers {
        head.push(name.len() as u8);
        head.extend_from_slice(name.as_bytes());
        head.push(7);
        head.extend_from_slice(&(value.len() as u16).to_be_bytes());
        head.extend_from_slice(value.as_bytes());
    }
    let total = PRELUDE + head.len() + payload.len() + 4;
    let mut out = Vec::new();
    out.extend_from_slice(&(total as u32).to_be_bytes());
    out.extend_from_slice(&(head.len() as u32).to_be_bytes());
    out.extend_from_slice(&[0; 4]);
    out.extend_from_slice(&head);
    out.extend_from_slice(payload);
    out.extend_from_slice(&[0; 4]);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decodes_split_frames_and_chunk_payloads() {
        let event = r#"{"type":"message_stop"}"#;
        let payload = serde_json::json!({
            "bytes": base64::engine::general_purpose::STANDARD.encode(event)
        })
        .to_string();
        let frame = encode(
            &[(":message-type", "event"), (":event-type", "chunk")],
            payload.as_bytes(),
        );
        let mut decoder = Decoder::default();
        let (first, second) = frame.split_at(7);
        assert!(decoder.push(first).unwrap().is_empty());
        let frames = decoder.push(second).unwrap();
        assert_eq!(frames.len(), 1);
        assert_eq!(chunk_json(&frames[0]).unwrap().as_deref(), Some(event));

        let error = encode(
            &[
                (":message-type", "exception"),
                (":exception-type", "throttlingException"),
            ],
            br#"{"message":"slow down"}"#,
        );
        let frames = Decoder::default().push(&error).unwrap();
        let err = chunk_json(&frames[0]).unwrap_err().to_string();
        assert!(
            err.contains("throttlingException") && err.contains("slow down"),
            "{err}"
        );
        assert!(
            Decoder::default()
                .push(&[0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0])
                .is_err()
        );
    }
}
