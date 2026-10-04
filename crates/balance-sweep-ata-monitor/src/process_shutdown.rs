//! Process-local signal admission and bounded shutdown. No durable state changes.
use std::{
    io,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc, Mutex,
    },
    thread,
    time::{Duration, Instant},
};
use tokio::task::AbortHandle;

static STOP: AtomicBool = AtomicBool::new(false);
static INSTALLED: AtomicBool = AtomicBool::new(false);
const POLL: Duration = Duration::from_millis(10);
// Leave time inside Render's original60s process stop allowance. Reaching this
// deadline is an incomplete drain, never a successful custody handoff.
const DRAIN_DEADLINE: Duration = Duration::from_secs(50);

fn incomplete_exit() -> ! {
    // Do not acquire stderr or run exit/flush hooks from the independent
    // watchdog: the stalled runtime can already hold those locks. This is a
    // failed drain; no task/pool/transaction completion is asserted.
    unsafe { libc::_exit(1) }
}

extern "C" fn request_stop(_: libc::c_int) {
    STOP.store(true, Ordering::SeqCst);
}

pub struct ProcessShutdown {
    running: Arc<AtomicBool>,
    state: Arc<Mutex<ShutdownState>>,
    watcher: Option<thread::JoinHandle<()>>,
}

struct ShutdownState {
    deadline: Option<Instant>,
    finished: bool,
    expired: bool,
}

impl ProcessShutdown {
    pub fn install(running: Arc<AtomicBool>) -> io::Result<Self> {
        if INSTALLED.swap(true, Ordering::SeqCst) {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "shutdown already installed",
            ));
        }
        for signal in [libc::SIGTERM, libc::SIGINT] {
            let mut action: libc::sigaction = unsafe { std::mem::zeroed() };
            action.sa_sigaction = request_stop as *const () as usize;
            unsafe { libc::sigemptyset(&mut action.sa_mask) };
            if unsafe { libc::sigaction(signal, &action, std::ptr::null_mut()) } != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        let state = Arc::new(Mutex::new(ShutdownState {
            deadline: None,
            finished: false,
            expired: false,
        }));
        let shared = state.clone();
        let gate = running.clone();
        let watcher = thread::Builder::new()
            .name("ata-shutdown".into())
            .spawn(move || {
                loop {
                    let mut state = shared.lock().unwrap_or_else(|_| incomplete_exit());
                    if state.finished {
                        break;
                    }
                    if STOP.load(Ordering::SeqCst) {
                        let until = *state
                            .deadline
                            .get_or_insert_with(|| Instant::now() + DRAIN_DEADLINE);
                        // Publish the deadline before opening the stop path. Completion
                        // and expiry are arbitrated by this same mutex.
                        gate.store(false, Ordering::SeqCst);
                        state.expired |= Instant::now() >= until;
                    }
                    let expired = state.expired;
                    drop(state);
                    if expired {
                        incomplete_exit();
                    }
                    thread::sleep(POLL);
                }
            })?;
        Ok(Self {
            running,
            state,
            watcher: Some(watcher),
        })
    }

    pub fn request(&self) {
        let mut state = self.state.lock().unwrap_or_else(|_| incomplete_exit());
        state
            .deadline
            .get_or_insert_with(|| Instant::now() + DRAIN_DEADLINE);
        STOP.store(true, Ordering::SeqCst);
        self.running.store(false, Ordering::SeqCst);
    }

    pub fn finish(mut self) -> io::Result<()> {
        if self.finish_watcher() {
            Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "ATA drain completed after its deadline",
            ))
        } else {
            Ok(())
        }
    }

    fn finish_watcher(&mut self) -> bool {
        let mut state = self.state.lock().unwrap_or_else(|_| incomplete_exit());
        state.expired |= state.deadline.is_some_and(|until| Instant::now() >= until);
        state.finished = true;
        let expired = state.expired;
        drop(state);
        if let Some(watcher) = self.watcher.take() {
            let _ = watcher.join();
        }
        expired
    }
}

impl Drop for ProcessShutdown {
    fn drop(&mut self) {
        // No completion receipt is emitted by Drop, including error paths.
        let _ = self.finish_watcher();
    }
}

pub async fn wait_for_stop(running: &AtomicBool) {
    while running.load(Ordering::SeqCst) {
        tokio::time::sleep(POLL).await;
    }
}

pub struct AbortOnDrop(AbortHandle);
impl AbortOnDrop {
    pub fn new(handle: AbortHandle) -> Self {
        Self(handle)
    }
}
impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        self.0.abort();
    }
}
