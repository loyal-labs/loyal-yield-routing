//! External file/protocol contract; loopback only, no RPC/DB/signing.
use futures_util::{future::BoxFuture, SinkExt, StreamExt};
use loyal_squads_policy_monitor::{
    BalanceSweepExecutionEvent, Cluster, Commitment, MonitorConfig, MonitorError, PolicyMatchSink,
    PolicyMonitor, PolicyMonitorEvent,
};
use serde_json::{json, Value};
use std::{
    path::{Path, PathBuf},
    time::{Duration, Instant},
};
use tokio_tungstenite::{accept_async, tungstenite::Message};

struct Sink;
impl PolicyMatchSink for Sink {
    fn emit(&mut self, _: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Ok(()) })
    }
    fn emit_execution(
        &mut self,
        _: BalanceSweepExecutionEvent,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Ok(()) })
    }
}

struct Directory(PathBuf);
impl Directory {
    fn new() -> Self {
        use std::os::unix::fs::PermissionsExt;
        let root = std::env::temp_dir().canonicalize().unwrap();
        let path = root.join(format!(
            "squads-health-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir(&path).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700)).unwrap();
        Self(path)
    }
    fn file(&self) -> PathBuf {
        self.0.join("health.json")
    }
}
impl Drop for Directory {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

async fn snapshot(path: &Path, predicate: impl Fn(&Value) -> bool) -> Value {
    let deadline = Instant::now() + Duration::from_secs(8);
    loop {
        if let Ok(bytes) = std::fs::read(path) {
            let value: Value = serde_json::from_slice(&bytes).expect("atomic complete JSON");
            if predicate(&value) {
                return value;
            }
        }
        assert!(Instant::now() < deadline, "health snapshot deadline");
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

#[tokio::test]
async fn quiet_pong_reconnect_and_writer_failure_are_not_fabricated() {
    let directory = Directory::new();
    let path = directory.file();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let config = MonitorConfig::new(
        Cluster::Mainnet,
        Commitment::Finalized,
        Some(format!("ws://{}", listener.local_addr().unwrap())),
        None,
    )
    .unwrap();
    let mut monitor = PolicyMonitor::new(config, Sink).with_health_path(Some(path.clone()));
    let task = tokio::spawn(async move { monitor.run(false).await });
    let (stream, _) = listener.accept().await.unwrap();
    let mut ws = accept_async(stream).await.unwrap();
    assert!(matches!(
        ws.next().await.unwrap().unwrap(),
        Message::Text(_)
    ));
    // Initial immediate tick, same empty payload as deployed source.
    assert!(matches!(ws.next().await.unwrap().unwrap(), Message::Ping(p) if p.is_empty()));
    // Wrong payload, invalid acknowledgments, and pre-ack Pong prove nothing.
    ws.send(Message::Pong(vec![1].into())).await.unwrap();
    ws.send(Message::Text(
        json!({"jsonrpc":"2.0","id":2,"result":41})
            .to_string()
            .into(),
    ))
    .await
    .unwrap();
    ws.send(Message::Text(
        json!({"jsonrpc":"2.0","id":1,"result":41,"error":null})
            .to_string()
            .into(),
    ))
    .await
    .unwrap();
    ws.send(Message::Pong(vec![].into())).await.unwrap();
    let unacked = snapshot(&path, |v| {
        v["connection_generation"] == 1 && v["connected"] == true
    })
    .await;
    assert_eq!(unacked["validated_pong_count"], 0);
    assert_eq!(unacked["subscription_acked"], false);
    assert!(unacked["last_pong_unix_ms"].is_null());
    ws.close(None).await.unwrap();
    let (stream, _) = listener.accept().await.unwrap();
    let mut ws = accept_async(stream).await.unwrap();
    assert!(matches!(
        ws.next().await.unwrap().unwrap(),
        Message::Text(_)
    ));
    assert!(matches!(ws.next().await.unwrap().unwrap(), Message::Ping(p) if p.is_empty()));
    ws.send(Message::Pong(vec![9].into())).await.unwrap(); // replace automatic Pong
    ws.send(Message::Text(
        json!({"jsonrpc":"2.0","id":1,"result":42})
            .to_string()
            .into(),
    ))
    .await
    .unwrap();
    ws.send(Message::Pong(vec![].into())).await.unwrap();
    let first = snapshot(&path, |v| v["validated_pong_count"] == 1).await;
    assert_eq!(first["connection_generation"], 2);
    assert_eq!(first["current_generation_pong_count"], 1);
    assert_eq!(first["subscription_acked"], true);
    assert_eq!(first["protocol_notification_count"], 0);
    assert!(first["last_finalized_output_slot"].is_null());
    ws.send(Message::Pong(vec![].into())).await.unwrap(); // unsolicited duplicate
    ws.send(Message::Text(json!({"jsonrpc":"2.0","method":"transactionNotification","params":{"subscription":42,"result":{}}}).to_string().into())).await.unwrap();
    let later = snapshot(&path, |v| {
        v["native_process_tick"].as_u64() > first["native_process_tick"].as_u64()
    })
    .await;
    for key in [
        "validated_pong_count",
        "last_pong_unix_ms",
        "last_pong_monotonic_ms",
        "protocol_notification_count",
    ] {
        assert_eq!(
            first[key], later[key],
            "publication must not fabricate {key}"
        );
    }
    ws.close(None).await.unwrap();
    let (stream, _) = listener.accept().await.unwrap();
    let mut ws = accept_async(stream).await.unwrap();
    ws.next().await.unwrap().unwrap();
    ws.next().await.unwrap().unwrap();
    ws.send(Message::Pong(vec![].into())).await.unwrap(); // old ack cannot carry over
    let reconnected = snapshot(&path, |v| {
        v["connection_generation"] == 3 && v["connected"] == true
    })
    .await;
    assert_eq!(reconnected["subscription_acked"], false);
    assert_eq!(reconnected["current_generation_pong_count"], 0);
    assert_eq!(reconnected["validated_pong_count"], 1);
    assert_eq!(reconnected["last_pong_generation"], 2);
    // A symlink target is refused; reporter invalidates it without touching its referent.
    let sentinel = directory.0.join("sentinel");
    std::fs::write(&sentinel, b"untouched").unwrap();
    std::fs::remove_file(&path).unwrap();
    std::os::unix::fs::symlink(&sentinel, &path).unwrap();
    let deadline = Instant::now() + Duration::from_secs(8);
    while std::fs::symlink_metadata(&path).is_ok_and(|m| m.file_type().is_symlink()) {
        assert!(Instant::now() < deadline);
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    let failed = snapshot(&path, |v| v["writer_failed"] == true).await;
    assert_eq!(failed["validated_pong_count"], 1);
    assert_eq!(std::fs::read(&sentinel).unwrap(), b"untouched");
    assert!(
        !task.is_finished(),
        "health IO must not terminate business processing"
    );
    task.abort();
    let _ = task.await;
}

struct FailingSink;
impl PolicyMatchSink for FailingSink {
    fn emit(&mut self, _: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Err(MonitorError::Decode("local sink failure".into())) })
    }
    fn emit_execution(
        &mut self,
        _: BalanceSweepExecutionEvent,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Err(MonitorError::Decode("local sink failure".into())) })
    }
}

async fn finalized_output<S: PolicyMatchSink + Send + Sync + 'static>(sink: S, success: bool) {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use solana_sdk::{
        message::{Message as SolanaMessage, VersionedMessage},
        pubkey::Pubkey,
        signature::Signature,
        transaction::VersionedTransaction,
    };
    let directory = Directory::new();
    let path = directory.file();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let config = MonitorConfig::new(
        Cluster::Mainnet,
        Commitment::Finalized,
        Some(format!("ws://{}", listener.local_addr().unwrap())),
        None,
    )
    .unwrap();
    let mut monitor = PolicyMonitor::new(config, sink).with_health_path(Some(path.clone()));
    let task = tokio::spawn(async move {
        let result = monitor.run(true).await;
        (monitor, result)
    });
    let (stream, _) = listener.accept().await.unwrap();
    let mut ws = accept_async(stream).await.unwrap();
    ws.next().await.unwrap().unwrap();
    ws.send(Message::Text(
        json!({"jsonrpc":"2.0","id":1,"result":42})
            .to_string()
            .into(),
    ))
    .await
    .unwrap();
    let payer = Pubkey::new_unique();
    let instruction =
        loyal_actions::remove_policy_instruction(Pubkey::new_unique(), payer, Pubkey::new_unique());
    let message = SolanaMessage::new(&[instruction], Some(&payer));
    let transaction = VersionedTransaction {
        signatures: vec![Signature::default(); usize::from(message.header.num_required_signatures)],
        message: VersionedMessage::Legacy(message),
    };
    let payload = STANDARD.encode(bincode::serialize(&transaction).unwrap());
    ws.send(Message::Text(json!({"jsonrpc":"2.0","method":"transactionNotification","params":{"subscription":42,"result":{"signature":"local-test-only","slot":123,"transaction":{"transaction":[payload,"base64"],"meta":{"err":null}}}}}).to_string().into())).await.unwrap();
    let (monitor, result) = tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(result.is_ok(), success);
    let value = snapshot(&path, |v| {
        v["protocol_notification_count"] == 1 && v["connected"] == false
    })
    .await;
    assert_eq!(value["last_protocol_notification_slot"], 123);
    if success {
        assert_eq!(value["last_finalized_output_slot"], 123);
    } else {
        assert!(value["last_finalized_output_slot"].is_null());
    }
    drop(monitor);
}

#[tokio::test]
async fn finalized_output_requires_successful_sink_completion() {
    finalized_output(Sink, true).await;
    finalized_output(FailingSink, false).await;
}
