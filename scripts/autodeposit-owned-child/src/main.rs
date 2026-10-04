#[path = "../../../crates/balance-sweep-autodeposit-trigger/src/executor_lifecycle.rs"]
mod executor_lifecycle;

#[tokio::main(flavor = "current_thread")]
async fn main() -> std::io::Result<()> {
    executor_lifecycle::install_shutdown_handlers()?;
    #[cfg(target_os = "linux")]
    {
        let mut subreaper: libc::c_int = 0;
        if unsafe { libc::prctl(libc::PR_GET_CHILD_SUBREAPER, &mut subreaper, 0, 0, 0) } != 0
            || subreaper != 1
        {
            return Err(std::io::Error::other("Linux subreaper is not effective"));
        }
        eprintln!(
            "probe identity pid={} euid={} sid={} pgid={} subreaper={subreaper}",
            std::process::id(),
            unsafe { libc::geteuid() },
            unsafe { libc::getsid(0) },
            unsafe { libc::getpgrp() }
        );
    }
    if std::env::args().nth(1).as_deref() == Some("--stop-before-spawn") {
        unsafe { libc::raise(libc::SIGTERM) };
        let mut command = std::process::Command::new("sh");
        command
            .arg("-c")
            .arg(std::env::args().nth(2).expect("synthetic command"));
        let result = executor_lifecycle::run_owned(command).await;
        if !executor_lifecycle::stopping()
            || !matches!(result, Err(ref e) if e.kind() == std::io::ErrorKind::Interrupted)
        {
            return Err(std::io::Error::other(
                "post-stop admission was not rejected",
            ));
        }
        return Ok(());
    }
    let mut command = std::process::Command::new("sh");
    command
        .arg("-c")
        .arg(std::env::args().nth(1).expect("synthetic command"));
    tokio::select! {
        biased;
        _ = executor_lifecycle::shutdown_requested() => Ok(()),
        result = executor_lifecycle::run_owned(command) => {
            match result {
                Ok(status) => std::process::exit(status.code().unwrap_or(1)),
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => Ok(()),
                Err(e) => { eprintln!("probe lifecycle errno={:?}", e.raw_os_error()); Err(e) },
            }
        },
    }
}
