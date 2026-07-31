use redis::{
    AsyncCommands,
    aio::MultiplexedConnection,
    streams::{StreamPendingReply, StreamRangeReply, StreamReadReply},
};
use uuid::Uuid;

const APPEND_SCRIPT: &str = include_str!("../../scripts/append_request_metadata.lua");
const DEAD_LETTER_SCRIPT: &str = include_str!("../../scripts/dead_letter_request_metadata.lua");

fn valkey_url() -> String {
    std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to an isolated test Valkey")
}

async fn connection() -> MultiplexedConnection {
    redis::Client::open(valkey_url())
        .unwrap()
        .get_multiplexed_async_connection()
        .await
        .unwrap()
}

fn keys(label: &str) -> (String, String, String) {
    let namespace = format!("olp:test:request-metadata:{label}:{}", Uuid::now_v7());
    (
        format!("{namespace}:stream"),
        format!("{namespace}:trimmed"),
        format!("{namespace}:receipts"),
    )
}

#[tokio::test]
#[ignore = "requires Valkey 9.0+ or Redis 7.4+ in OLP_VALKEY_URL (CI/deploy use Valkey 9.1)"]
async fn append_is_idempotent_and_preflights_trim_counter() {
    let mut connection = connection().await;
    let (stream, counter, receipts) = keys("append");
    let event_id = Uuid::now_v7().to_string();

    let first: (String, u64) = redis::Script::new(APPEND_SCRIPT)
        .key(&stream)
        .key(&counter)
        .key(&receipts)
        .arg(r#"{"attempt":1}"#)
        .arg(10)
        .arg(&event_id)
        .arg(60)
        .invoke_async(&mut connection)
        .await
        .unwrap();
    redis::cmd("HEXPIRE")
        .arg(&receipts)
        .arg(1)
        .arg("FIELDS")
        .arg(1)
        .arg(&event_id)
        .query_async::<Vec<i64>>(&mut connection)
        .await
        .unwrap();
    let before_refresh = receipt_ttl(&mut connection, &receipts, &event_id).await;
    let retry: (String, u64) = redis::Script::new(APPEND_SCRIPT)
        .key(&stream)
        .key(&counter)
        .key(&receipts)
        .arg(r#"{"attempt":2}"#)
        .arg(10)
        .arg(&event_id)
        .arg(60)
        .invoke_async(&mut connection)
        .await
        .unwrap();
    assert_eq!(retry, first);
    assert!(
        receipt_ttl(&mut connection, &receipts, &event_id).await > before_refresh,
        "an ambiguous append retry must refresh its deduplication receipt"
    );
    assert_eq!(connection.xlen::<_, usize>(&stream).await.unwrap(), 1);

    connection
        .set::<_, _, ()>(&counter, "not-an-integer")
        .await
        .unwrap();
    let error = redis::Script::new(APPEND_SCRIPT)
        .key(&stream)
        .key(&counter)
        .key(&receipts)
        .arg(r#"{"attempt":3}"#)
        .arg(10)
        .arg(Uuid::now_v7().to_string())
        .arg(60)
        .invoke_async::<String>(&mut connection)
        .await
        .unwrap_err();
    assert!(error.to_string().contains("trim counter"));
    assert_eq!(connection.xlen::<_, usize>(&stream).await.unwrap(), 1);
}

#[tokio::test]
#[ignore = "requires Valkey 9.0+ or Redis 7.4+ in OLP_VALKEY_URL (CI/deploy use Valkey 9.1)"]
async fn dead_letter_failure_preserves_the_pending_source() {
    let mut connection = connection().await;
    let (source, dead_letter, _) = keys("dead-letter");
    let group = "persistence";
    let source_id: String = redis::cmd("XADD")
        .arg(&source)
        .arg("*")
        .arg("event")
        .arg("{}")
        .query_async(&mut connection)
        .await
        .unwrap();
    redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(&source)
        .arg(group)
        .arg("0")
        .query_async::<()>(&mut connection)
        .await
        .unwrap();
    let _: StreamReadReply = redis::cmd("XREADGROUP")
        .arg("GROUP")
        .arg(group)
        .arg("worker")
        .arg("COUNT")
        .arg(1)
        .arg("STREAMS")
        .arg(&source)
        .arg(">")
        .query_async(&mut connection)
        .await
        .unwrap();

    connection
        .set::<_, _, ()>(&dead_letter, "wrong-type")
        .await
        .unwrap();
    redis::Script::new(DEAD_LETTER_SCRIPT)
        .key(&source)
        .key(&dead_letter)
        .arg(group)
        .arg(&source_id)
        .arg("{}")
        .arg(10)
        .invoke_async::<String>(&mut connection)
        .await
        .unwrap_err();
    let source_entries: StreamRangeReply = connection
        .xrange(&source, &source_id, &source_id)
        .await
        .unwrap();
    assert_eq!(source_entries.ids.len(), 1);
    assert_eq!(pending_count(&mut connection, &source, group).await, 1);

    connection.del::<_, ()>(&dead_letter).await.unwrap();
    let _: String = redis::Script::new(DEAD_LETTER_SCRIPT)
        .key(&source)
        .key(&dead_letter)
        .arg(group)
        .arg(&source_id)
        .arg("{}")
        .arg(10)
        .invoke_async(&mut connection)
        .await
        .unwrap();
    assert_eq!(connection.xlen::<_, usize>(&source).await.unwrap(), 0);
    assert_eq!(connection.xlen::<_, usize>(&dead_letter).await.unwrap(), 1);
    assert_eq!(pending_count(&mut connection, &source, group).await, 0);

    let unclaimed_id: String = redis::cmd("XADD")
        .arg(&source)
        .arg("*")
        .arg("event")
        .arg("{}")
        .query_async(&mut connection)
        .await
        .unwrap();
    redis::Script::new(DEAD_LETTER_SCRIPT)
        .key(&source)
        .key(&dead_letter)
        .arg(group)
        .arg(&unclaimed_id)
        .arg("{}")
        .arg(10)
        .invoke_async::<String>(&mut connection)
        .await
        .unwrap_err();
    assert_eq!(connection.xlen::<_, usize>(&source).await.unwrap(), 1);
    assert_eq!(connection.xlen::<_, usize>(&dead_letter).await.unwrap(), 1);
}

async fn receipt_ttl(
    connection: &mut MultiplexedConnection,
    receipts: &str,
    event_id: &str,
) -> i64 {
    redis::cmd("HTTL")
        .arg(receipts)
        .arg("FIELDS")
        .arg(1)
        .arg(event_id)
        .query_async::<Vec<i64>>(connection)
        .await
        .unwrap()[0]
}

async fn pending_count(connection: &mut MultiplexedConnection, stream: &str, group: &str) -> usize {
    match connection
        .xpending::<_, _, StreamPendingReply>(stream, group)
        .await
        .unwrap()
    {
        StreamPendingReply::Empty => 0,
        StreamPendingReply::Data(data) => data.count,
        _ => unreachable!("unexpected pending reply variant"),
    }
}
