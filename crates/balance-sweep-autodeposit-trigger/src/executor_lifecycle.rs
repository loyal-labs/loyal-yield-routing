//! Unix-only owned executor lifecycle. No journal/accounting mutations.
use std::{
    io,
    os::unix::process::CommandExt,
    process::{Child, Command, ExitStatus},
    sync::atomic::{AtomicBool, Ordering},
    time::{Duration, Instant},
};

static STOP: AtomicBool = AtomicBool::new(false);
const POLL: Duration = Duration::from_millis(20);
const TERM_GRACE: Duration = Duration::from_secs(20);
const KILL_GRACE: Duration = Duration::from_secs(2);

extern "C" fn request_stop(_: libc::c_int) {
    STOP.store(true, Ordering::SeqCst);
}

pub fn stopping() -> bool {
    STOP.load(Ordering::SeqCst)
}

pub fn install_shutdown_handlers() -> io::Result<()> {
    // Linux production: adopt orphaned executor grandchildren so group drain
    // can wait/reap them instead of merely dispatching a signal.
    #[cfg(target_os = "linux")]
    if unsafe { libc::prctl(libc::PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) } != 0 {
        return Err(io::Error::last_os_error());
    }
    for signal in [libc::SIGTERM, libc::SIGINT] {
        // Only an atomic store in the handler; no allocation, locks or logging.
        let mut action: libc::sigaction = unsafe { std::mem::zeroed() };
        action.sa_sigaction = request_stop as *const () as usize;
        unsafe { libc::sigemptyset(&mut action.sa_mask) };
        if unsafe { libc::sigaction(signal, &action, std::ptr::null_mut()) } != 0 {
            return Err(io::Error::last_os_error());
        }
    }
    Ok(())
}

pub async fn shutdown_requested() {
    while !STOP.load(Ordering::SeqCst) {
        tokio::time::sleep(POLL).await;
    }
}

// Keep the group leader unreaped until group signaling is complete: its PID
// cannot be recycled into an unrelated process group during drain.
struct OwnedChild {
    child: Child,
    expected_euid: libc::uid_t,
    expected_session: libc::pid_t,
    group: libc::pid_t,
}
impl OwnedChild {
    fn exited(&self) -> io::Result<bool> {
        let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
        let rc = unsafe {
            libc::waitid(
                libc::P_PID,
                self.child.id() as libc::id_t,
                &mut info,
                libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
            )
        };
        if rc != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(unsafe { info.si_pid() } != 0)
    }

    fn signal_group(&self, signal: libc::c_int) -> io::Result<()> {
        let pid = self.child.id() as libc::pid_t;
        let euid = unsafe { libc::geteuid() };
        let session = unsafe { libc::getsid(pid) };
        let group = unsafe { libc::getpgid(pid) };
        // Leader remains unreaped, so this identity cannot refer to a reused PID.
        if euid != self.expected_euid || session != self.expected_session || group != self.group {
            eprintln!(
                "autodeposit owned-group identity rejected: pid={pid} expected_euid={} euid={euid} expected_sid={} sid={session} expected_pgid={} pgid={group}",
                self.expected_euid, self.expected_session, self.group,
            );
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "owned group identity mismatch",
            ));
        }
        #[cfg(target_os = "linux")]
        {
            let status = std::fs::read_to_string(format!("/proc/{pid}/status"))?;
            let uid = status
                .lines()
                .find_map(|line| line.strip_prefix("Uid:"))
                .and_then(|line| line.split_whitespace().nth(1))
                .and_then(|value| value.parse::<libc::uid_t>().ok());
            if uid != Some(self.expected_euid) {
                return Err(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "owned leader effective UID mismatch",
                ));
            }
        }
        let rc = unsafe { libc::kill(-self.group, signal) };
        if rc == 0 || io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH) {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }
}

impl Drop for OwnedChild {
    fn drop(&mut self) {
        // Always clean remaining descendants, even if the shell exited first.
        // A cancelled async future cannot abandon the child group.
        let term_ok = self.signal_group(libc::SIGTERM).is_ok();
        let until = Instant::now() + TERM_GRACE;
        while term_ok && Instant::now() < until {
            if self.exited().unwrap_or(false) {
                break;
            }
            std::thread::sleep(POLL);
        }
        if let Err(error) = self.signal_group(libc::SIGKILL) {
            eprintln!(
                "autodeposit owned-group kill failed: errno={:?}",
                error.raw_os_error()
            );
            std::process::exit(1);
        }
        let until = Instant::now() + KILL_GRACE;
        while Instant::now() < until {
            if self.exited().unwrap_or(false) {
                // Reap only after the final group signal, never before.
                if self.child.wait().is_err() {
                    std::process::exit(1);
                }
                #[cfg(target_os = "linux")]
                loop {
                    let rc = unsafe {
                        libc::waitpid(
                            -(self.child.id() as libc::pid_t),
                            std::ptr::null_mut(),
                            libc::WNOHANG,
                        )
                    };
                    if rc < 0 && io::Error::last_os_error().raw_os_error() == Some(libc::ECHILD) {
                        break;
                    }
                    if rc < 0 || Instant::now() >= until {
                        std::process::exit(1);
                    }
                    if rc == 0 {
                        std::thread::sleep(POLL);
                    }
                }
                return;
            }
            std::thread::sleep(POLL);
        }
        // Fail closed: no subsequent scan or successful shutdown claim. Host
        // custody must resolve an uninterruptible child; no journal is released.
        eprintln!("autodeposit owned-group reap timed out");
        std::process::exit(1);
    }
}

pub async fn run_owned(mut command: Command) -> io::Result<ExitStatus> {
    if STOP.load(Ordering::SeqCst) {
        return Err(io::Error::new(
            io::ErrorKind::Interrupted,
            "admission stopped",
        ));
    }
    command.process_group(0);
    let expected_euid = unsafe { libc::geteuid() };
    let expected_session = unsafe { libc::getsid(0) };
    let leader = command.spawn()?;
    let group = leader.id() as libc::pid_t;
    let child = OwnedChild {
        child: leader,
        expected_euid,
        expected_session,
        group,
    };
    loop {
        if STOP.load(Ordering::SeqCst) {
            // Drop drains group; caller never classifies this as an exit outcome.
            return Err(io::Error::new(
                io::ErrorKind::Interrupted,
                "executor interrupted",
            ));
        }
        if child.exited()? {
            // Read status without reaping so Drop retains the group identity.
            let mut info: libc::siginfo_t = unsafe { std::mem::zeroed() };
            if unsafe {
                libc::waitid(
                    libc::P_PID,
                    child.child.id() as libc::id_t,
                    &mut info,
                    libc::WEXITED | libc::WNOWAIT,
                )
            } != 0
            {
                return Err(io::Error::last_os_error());
            }
            use std::os::unix::process::ExitStatusExt;
            let raw = match info.si_code {
                libc::CLD_EXITED => (unsafe { info.si_status() }) << 8,
                libc::CLD_DUMPED => (unsafe { info.si_status() }) | 0x80,
                _ => unsafe { info.si_status() },
            };
            return Ok(ExitStatus::from_raw(raw));
        }
        tokio::time::sleep(POLL).await;
    }
}
