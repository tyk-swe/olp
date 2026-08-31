use std::collections::HashSet;

use redis::Value;

use super::super::Error;

#[derive(Debug)]
pub(super) struct StreamEntry {
    pub(super) id: String,
    pub(super) payload: Option<Vec<u8>>,
    pub(super) deleted_pending_id: Option<String>,
}

#[derive(Debug)]
pub(super) struct AutoClaimPage {
    pub(super) next_start: String,
    pub(super) entries: Vec<StreamEntry>,
    pub(super) deleted_ids: Vec<String>,
}

pub(super) fn parse_xread_reply(
    reply: Value,
    expected_stream: &str,
    batch_size: usize,
) -> Result<Vec<StreamEntry>, Error> {
    let (stream, entries) = match reply {
        Value::Nil => return Ok(Vec::new()),
        Value::Array(mut streams) if streams.len() == 1 => {
            let stream = streams.pop().expect("one stream reply was validated");
            let Value::Array(mut pair) = stream else {
                return Err(Error::InvalidState("invalid XREADGROUP stream tuple"));
            };
            if pair.len() != 2 {
                return Err(Error::InvalidState(
                    "invalid XREADGROUP stream tuple length",
                ));
            }
            let entries = pair.pop().expect("stream tuple length was validated");
            let stream = pair.pop().expect("stream tuple length was validated");
            (stream, entries)
        }
        Value::Map(mut streams) if streams.len() == 1 => {
            streams.pop().expect("one stream map entry was validated")
        }
        _ => {
            return Err(Error::InvalidState("invalid XREADGROUP reply"));
        }
    };
    if value_bytes(stream).as_deref() != Some(expected_stream.as_bytes()) {
        return Err(Error::InvalidState(
            "XREADGROUP returned an unexpected stream",
        ));
    }
    parse_entries(entries, batch_size)
}

pub(super) fn parse_auto_claim_reply(
    reply: Value,
    batch_size: usize,
) -> Result<AutoClaimPage, Error> {
    let Value::Array(mut items) = reply else {
        return Err(Error::InvalidState("invalid XAUTOCLAIM reply"));
    };
    if !(2..=3).contains(&items.len()) {
        return Err(Error::InvalidState("invalid XAUTOCLAIM reply length"));
    }
    let deleted = if items.len() == 3 {
        items.pop().expect("XAUTOCLAIM reply length was validated")
    } else {
        Value::Array(Vec::new())
    };
    let entries = items.pop().expect("XAUTOCLAIM reply length was validated");
    let next_start = value_string(items.pop().expect("XAUTOCLAIM reply length was validated"))?;
    parse_stream_id(&next_start)?;
    let entries = parse_entries(entries, batch_size)?;
    let deleted_ids = parse_id_list(deleted)?;
    if deleted_ids.len() > batch_size.saturating_mul(10) {
        return Err(Error::InvalidState(
            "XAUTOCLAIM deleted-ID scan exceeded its protocol bound",
        ));
    }
    let claimed_ids = entries
        .iter()
        .map(|entry| entry.id.as_str())
        .collect::<HashSet<_>>();
    if deleted_ids
        .iter()
        .any(|id| claimed_ids.contains(id.as_str()))
    {
        return Err(Error::InvalidState(
            "XAUTOCLAIM returned overlapping claimed and deleted IDs",
        ));
    }
    Ok(AutoClaimPage {
        next_start,
        entries,
        deleted_ids,
    })
}

fn parse_entries(entries: Value, batch_size: usize) -> Result<Vec<StreamEntry>, Error> {
    let Value::Array(entries) = entries else {
        return Err(Error::InvalidState("invalid stream entry list"));
    };
    if entries.len() > batch_size {
        return Err(Error::InvalidState(
            "stream reply exceeded the requested batch size",
        ));
    }
    let mut ids = HashSet::with_capacity(entries.len());
    entries
        .into_iter()
        .map(|entry| {
            let Value::Array(mut fields) = entry else {
                return Err(Error::InvalidState("invalid stream entry tuple"));
            };
            if fields.len() != 2 {
                return Err(Error::InvalidState("invalid stream entry tuple length"));
            }
            let field_values = fields.pop().expect("entry tuple length was validated");
            let id = value_string(fields.pop().expect("entry tuple length was validated"))?;
            parse_stream_id(&id)?;
            if !ids.insert(id.clone()) {
                return Err(Error::InvalidState(
                    "stream reply contained a duplicate entry ID",
                ));
            }
            let (payload, deleted_pending_id) = parse_entry_fields(field_values)?;
            Ok(StreamEntry {
                id,
                payload,
                deleted_pending_id,
            })
        })
        .collect()
}

fn parse_entry_fields(fields: Value) -> Result<(Option<Vec<u8>>, Option<String>), Error> {
    let pairs = match fields {
        Value::Nil => return Ok((None, None)),
        Value::Array(values) => {
            if values.len() % 2 != 0 {
                return Err(Error::InvalidState("stream field list has odd length"));
            }
            let mut values = values.into_iter();
            let mut pairs = Vec::with_capacity(values.len() / 2);
            while let Some(field) = values.next() {
                let value = values.next().expect("field list length was validated");
                pairs.push((field, value));
            }
            pairs
        }
        Value::Map(pairs) => pairs,
        _ => {
            return Err(Error::InvalidState("invalid stream field container"));
        }
    };
    if pairs.len() != 1 {
        return Ok((None, None));
    }
    let (field, value) = pairs
        .into_iter()
        .next()
        .expect("one stream field was validated");
    match value_bytes(field).as_deref() {
        Some(b"event") => Ok((value_bytes(value), None)),
        Some(b"deleted_pending_id") => {
            let id = value_string(value)?;
            parse_stream_id(&id)?;
            Ok((None, Some(id)))
        }
        _ => Ok((None, None)),
    }
}

fn parse_id_list(value: Value) -> Result<Vec<String>, Error> {
    let Value::Array(values) = value else {
        return Err(Error::InvalidState("invalid XAUTOCLAIM deleted-ID list"));
    };
    let mut ids = HashSet::with_capacity(values.len());
    values
        .into_iter()
        .map(|value| {
            let id = value_string(value)?;
            parse_stream_id(&id)?;
            if !ids.insert(id.clone()) {
                return Err(Error::InvalidState(
                    "XAUTOCLAIM returned a duplicate deleted ID",
                ));
            }
            Ok(id)
        })
        .collect()
}

fn value_string(value: Value) -> Result<String, Error> {
    let bytes = value_bytes(value).ok_or(Error::InvalidState(
        "stream reply contained a non-string value",
    ))?;
    String::from_utf8(bytes)
        .map_err(|_| Error::InvalidState("stream reply contained invalid UTF-8"))
}

fn value_bytes(value: Value) -> Option<Vec<u8>> {
    match value {
        Value::BulkString(value) => Some(value),
        Value::SimpleString(value) => Some(value.into_bytes()),
        _ => None,
    }
}

pub(super) fn parse_stream_id(id: &str) -> Result<(u64, u64), Error> {
    let (milliseconds, sequence) = id
        .split_once('-')
        .ok_or(Error::InvalidState("stream reply contained an invalid ID"))?;
    if milliseconds.is_empty()
        || sequence.is_empty()
        || !milliseconds.bytes().all(|byte| byte.is_ascii_digit())
        || !sequence.bytes().all(|byte| byte.is_ascii_digit())
    {
        return Err(Error::InvalidState("stream reply contained an invalid ID"));
    }
    Ok((
        milliseconds
            .parse()
            .map_err(|_| Error::InvalidState("stream reply contained an overflowing ID"))?,
        sequence
            .parse()
            .map_err(|_| Error::InvalidState("stream reply contained an overflowing ID"))?,
    ))
}

#[cfg(test)]
mod tests;
