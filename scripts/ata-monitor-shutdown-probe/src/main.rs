//! Real process components only; no database, RPC, secrets or financial fixtures.
#[path = "../../../crates/balance-sweep-ata-monitor/src/process_shutdown.rs"]
mod process_shutdown;
use process_shutdown::{wait_for_stop, AbortOnDrop, ProcessShutdown};
use std::{
    io::{self, Write},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

struct MarkDropped(Arc<AtomicBool>);
impl Drop for MarkDropped {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mode = std::env::args().nth(1).ok_or("mode required")?;
    if mode == "abort-wrapper" {
        let dropped = Arc::new(AtomicBool::new(false));
        let mark = dropped.clone();
        let (ready_tx, ready_rx) = tokio::sync::oneshot::channel();
        let inner = tokio::spawn(async move {
            let _mark = MarkDropped(mark);
            let _ = ready_tx.send(());
            std::future::pending::<()>().await;
        });
        let guard = AbortOnDrop::new(inner.abort_handle());
        let outer = tokio::spawn(async move {
            let _guard = guard;
            inner.await
        });
        tokio::time::timeout(Duration::from_secs(2), ready_rx).await??;
        outer.abort();
        assert!(outer.await.unwrap_err().is_cancelled());
        tokio::time::timeout(Duration::from_secs(2), async {
            while !dropped.load(Ordering::SeqCst) {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await?;
        println!("INNER_ABORTED");
        return Ok(());
    }
    if mode != "signal" && mode != "blocked-runtime" {
        return Err("unknown mode".into());
    }
    let running = Arc::new(AtomicBool::new(true));
    let shutdown = ProcessShutdown::install(running.clone())?;
    println!("READY");
    io::stdout().flush()?;
    if mode == "blocked-runtime" {
        // The Tokio current-thread runtime cannot poll a timer here. Its separate
        // production watchdog must still exit nonzero after the real deadline.
        let _stderr_lock = io::stderr().lock();
        std::thread::sleep(Duration::from_secs(120));
    } else {
        wait_for_stop(&running).await;
    }
    assert!(!running.load(Ordering::SeqCst));
    shutdown.request();
    shutdown.finish()?;
    println!("STOPPED");
    Ok(())
}
