use loyal_yield_orchestrator::{WorkerRuntime, WorkerRuntimeConfig};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = WorkerRuntimeConfig::from_env()?;
    let runtime = WorkerRuntime::connect(config).await?;
    runtime.run_until_shutdown().await?;
    Ok(())
}
