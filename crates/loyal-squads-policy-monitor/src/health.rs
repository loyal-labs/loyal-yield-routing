//! Secret-free observations, never an upstream health verdict.
use serde::Serialize;
use std::{
    fs::File,
    io::{self, Write},
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

#[derive(Clone, Serialize)]
pub(crate) struct State {
    schema: &'static str,
    version: u32,
    pid: u32,
    process_start_ticks: Option<u64>,
    runtime_epoch_unix_ns: u128,
    pub connected: bool,
    pub reconnecting: bool,
    pub connection_generation: u64,
    pub subscription_acked: bool,
    pub current_generation_pong_count: u64,
    validated_pong_count: u64,
    last_pong_generation: Option<u64>,
    last_pong_unix_ms: Option<u128>,
    last_pong_monotonic_ms: Option<u128>,
    pub protocol_notification_count: u64,
    pub last_protocol_notification_slot: Option<u64>,
    pub last_finalized_output_slot: Option<u64>,
    writer_failed: bool,
    native_process_tick: u64,
    snapshot_monotonic_ms: u128,
}

pub(crate) struct Health {
    state: Arc<Mutex<State>>,
    epoch: Instant,
}

impl Health {
    pub fn start(path: PathBuf) -> Option<Self> {
        let destination = match Destination::open(path) {
            Ok(destination) => destination,
            Err(_) => {
                eprintln!("policy monitor health writer unavailable");
                return None;
            }
        };
        let epoch = Instant::now();
        let state = Arc::new(Mutex::new(State {
            schema: "loyal_squads_policy_monitor_health",
            version: 1,
            pid: std::process::id(),
            process_start_ticks: process_start_ticks(),
            runtime_epoch_unix_ns: unix_elapsed().as_nanos(),
            connected: false,
            reconnecting: true,
            connection_generation: 0,
            subscription_acked: false,
            current_generation_pong_count: 0,
            validated_pong_count: 0,
            last_pong_generation: None,
            last_pong_unix_ms: None,
            last_pong_monotonic_ms: None,
            protocol_notification_count: 0,
            last_protocol_notification_slot: None,
            last_finalized_output_slot: None,
            writer_failed: false,
            native_process_tick: 0,
            snapshot_monotonic_ms: 0,
        }));
        let weak = Arc::downgrade(&state);
        let spawned = std::thread::Builder::new()
            .name("policy-health".into())
            .spawn(move || {
                loop {
                    let Some(shared) = weak.upgrade() else { break };
                    let mut snapshot = match shared.lock() {
                        Ok(mut state) => {
                            state.native_process_tick += 1;
                            state.snapshot_monotonic_ms = epoch.elapsed().as_millis();
                            state.clone()
                        }
                        Err(_) => break,
                    };
                    if destination.write(&snapshot).is_err() {
                        if !snapshot.writer_failed {
                            eprintln!("policy monitor health writer failed");
                        }
                        snapshot.writer_failed = true;
                        if let Ok(mut state) = shared.lock() {
                            state.writer_failed = true;
                        }
                        // Best effort invalidation; if impossible, readers must reject stale data.
                        destination.remove();
                    }
                    drop(shared);
                    std::thread::sleep(Duration::from_secs(1));
                }
            });
        if spawned.is_err() {
            eprintln!("policy monitor health writer unavailable");
            return None;
        }
        Some(Self { state, epoch })
    }

    pub fn update(&self, update: impl FnOnce(&mut State)) {
        if let Ok(mut state) = self.state.lock() {
            update(&mut state);
        }
    }

    pub fn pong(&self) {
        // Capture arrival before taking the state lock, never during publication.
        let wall = unix_elapsed().as_millis();
        let monotonic = self.epoch.elapsed().as_millis();
        self.update(|state| {
            if state.connected && state.subscription_acked {
                state.validated_pong_count += 1;
                state.current_generation_pong_count += 1;
                state.last_pong_generation = Some(state.connection_generation);
                state.last_pong_unix_ms = Some(wall);
                state.last_pong_monotonic_ms = Some(monotonic);
            }
        });
    }
}

fn unix_elapsed() -> Duration {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
}

fn process_start_ticks() -> Option<u64> {
    #[cfg(target_os = "linux")]
    {
        let stat = std::fs::read_to_string("/proc/self/stat").ok()?;
        // comm can contain spaces and parentheses. Fields after its final ')' start at 3.
        return stat
            .rsplit_once(')')?
            .1
            .split_whitespace()
            .nth(19)?
            .parse()
            .ok();
    }
    #[cfg(not(target_os = "linux"))]
    None
}

#[cfg(unix)]
struct Destination {
    directory: File,
    name: std::ffi::CString,
}

#[cfg(unix)]
impl Destination {
    fn open(path: PathBuf) -> io::Result<Self> {
        use std::os::{
            fd::{AsRawFd, FromRawFd},
            unix::fs::MetadataExt,
        };
        use std::path::Component;
        let invalid = || io::Error::from(io::ErrorKind::PermissionDenied);
        if !path.is_absolute() {
            return Err(invalid());
        }
        let name = path.file_name().ok_or_else(invalid)?;
        use std::os::unix::ffi::OsStrExt;
        let name = std::ffi::CString::new(name.as_bytes()).map_err(|_| invalid())?;
        let parent = path.parent().ok_or_else(invalid)?;
        let mut directory = File::open("/")?;
        // Walk via held descriptors: no symlink traversal or pathname race.
        let uid = unsafe { libc::geteuid() };
        for component in parent.components() {
            match component {
                Component::RootDir => continue,
                Component::Normal(name) => {
                    let name = std::ffi::CString::new(name.as_bytes()).map_err(|_| invalid())?;
                    let fd = unsafe {
                        libc::openat(
                            directory.as_raw_fd(),
                            name.as_ptr(),
                            libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                        )
                    };
                    if fd < 0 {
                        return Err(io::Error::last_os_error());
                    }
                    directory = unsafe { File::from_raw_fd(fd) };
                    let metadata = directory.metadata()?;
                    let mode = metadata.mode();
                    // Root-owned sticky ancestors (e.g. /tmp) are acceptable; leaf isn't.
                    let sticky_root = metadata.uid() == 0 && mode & u32::from(libc::S_ISVTX) != 0;
                    if (metadata.uid() != 0 && metadata.uid() != uid)
                        || (mode & 0o022 != 0 && !sticky_root)
                    {
                        return Err(invalid());
                    }
                }
                _ => return Err(invalid()),
            }
        }
        let metadata = directory.metadata()?;
        if metadata.mode() & 0o022 != 0 {
            return Err(invalid());
        }
        let destination = Self { directory, name };
        destination.validate_target()?;
        Ok(destination)
    }

    fn validate_target(&self) -> io::Result<()> {
        use std::os::fd::AsRawFd;
        let mut stat = std::mem::MaybeUninit::<libc::stat>::uninit();
        let result = unsafe {
            libc::fstatat(
                self.directory.as_raw_fd(),
                self.name.as_ptr(),
                stat.as_mut_ptr(),
                libc::AT_SYMLINK_NOFOLLOW,
            )
        };
        if result != 0 {
            let error = io::Error::last_os_error();
            return if error.kind() == io::ErrorKind::NotFound {
                Ok(())
            } else {
                Err(error)
            };
        }
        let stat = unsafe { stat.assume_init() };
        if stat.st_mode & libc::S_IFMT != libc::S_IFREG || stat.st_uid != unsafe { libc::geteuid() }
        {
            return Err(io::Error::from(io::ErrorKind::PermissionDenied));
        }
        Ok(())
    }

    fn write(&self, state: &State) -> io::Result<()> {
        use std::os::fd::{AsRawFd, FromRawFd};
        self.validate_target()?;
        let bytes = serde_json::to_vec(state)?;
        let temporary = std::ffi::CString::new(format!(
            ".policy-health-{}-{}",
            state.pid, state.runtime_epoch_unix_ns
        ))
        .unwrap();
        let fd = unsafe {
            libc::openat(
                self.directory.as_raw_fd(),
                temporary.as_ptr(),
                libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                0o600,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let mut file = unsafe { File::from_raw_fd(fd) };
        let result = (|| {
            file.write_all(&bytes)?;
            file.sync_all()?;
            if unsafe {
                libc::renameat(
                    self.directory.as_raw_fd(),
                    temporary.as_ptr(),
                    self.directory.as_raw_fd(),
                    self.name.as_ptr(),
                )
            } != 0
            {
                return Err(io::Error::last_os_error());
            }
            Ok(())
        })();
        if result.is_err() {
            unsafe {
                libc::unlinkat(self.directory.as_raw_fd(), temporary.as_ptr(), 0);
            }
        }
        result
    }

    fn remove(&self) {
        use std::os::fd::AsRawFd;
        unsafe {
            libc::unlinkat(self.directory.as_raw_fd(), self.name.as_ptr(), 0);
        }
    }
}

#[cfg(not(unix))]
struct Destination;
#[cfg(not(unix))]
impl Destination {
    fn open(_: PathBuf) -> io::Result<Self> {
        Err(io::ErrorKind::Unsupported.into())
    }
    fn write(&self, _: &State) -> io::Result<()> {
        Err(io::ErrorKind::Unsupported.into())
    }
    fn remove(&self) {}
}
