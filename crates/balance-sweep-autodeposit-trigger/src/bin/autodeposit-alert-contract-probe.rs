use balance_sweep_autodeposit_trigger::executor_failure_alert;
use loyal_observability::{init, ObservabilityConfig, OperationalError};

fn main() {
    let exit_code = std::env::args()
        .nth(1)
        .expect("exit code")
        .parse::<i32>()
        .expect("numeric exit code");
    let alert = executor_failure_alert(Some(exit_code));
    let config = ObservabilityConfig::from_env("autodeposit-alert-contract-probe")
        .expect("read observability configuration");
    let service_version = config.service_version.clone();
    let _guard = init(config).expect("initialize local observability");
    OperationalError::new(alert.code, alert.operation, alert.summary)
        .recovery_required(true)
        .emit();
    println!(
        "VERIFIER_RESULT={}",
        serde_json::json!({
            "code": alert.code,
            "operation": alert.operation,
            "summary": alert.summary,
            "serviceVersion": service_version,
        })
    );
}
