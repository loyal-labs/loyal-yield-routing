#[path = "../src/health.rs"]
mod health;
#[cfg(test)]
mod tests {
    use super::health::Health;
    use std::{
        fs,
        path::PathBuf,
        time::{Duration, Instant},
    };
    fn directory() -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let p = std::env::temp_dir().canonicalize().unwrap().join(format!(
            "health-unit-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&p).unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
        p
    }
    fn snapshot(
        path: &std::path::Path,
        test: impl Fn(&serde_json::Value) -> bool,
    ) -> serde_json::Value {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Ok(b) = fs::read(path) {
                if let Ok(v) = serde_json::from_slice(&b) {
                    if test(&v) {
                        return v;
                    }
                }
            }
            assert!(Instant::now() < deadline);
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    #[test]
    fn arrival_survives_publication_writer_failure_is_sticky_and_restart_resets() {
        let dir = directory();
        let path = dir.join("health.json");
        let h = Health::start(path.clone()).unwrap();
        h.pong(); // unacked must not count
        let initial = snapshot(&path, |_| true);
        assert_eq!(initial["validated_pong_count"], 0);
        h.update(|s| {
            s.connected = true;
            s.subscription_acked = true;
            s.connection_generation = 1;
        });
        h.pong();
        let first = snapshot(&path, |v| v["validated_pong_count"] == 1);
        let later = snapshot(&path, |v| {
            v["native_process_tick"].as_u64() > first["native_process_tick"].as_u64()
        });
        assert_eq!(first["last_pong_unix_ms"], later["last_pong_unix_ms"]);
        assert_eq!(
            first["last_pong_monotonic_ms"],
            later["last_pong_monotonic_ms"]
        );
        fs::remove_file(&path).unwrap();
        fs::create_dir(&path).unwrap();
        std::thread::sleep(Duration::from_millis(1200));
        fs::remove_dir(&path).unwrap();
        let failed = snapshot(&path, |v| v["writer_failed"] == true);
        assert_eq!(failed["validated_pong_count"], 1);
        drop(h);
        std::thread::sleep(Duration::from_millis(1100));
        let restarted = Health::start(path.clone()).unwrap();
        let fresh = snapshot(&path, |v| {
            v["runtime_epoch_unix_ns"] != first["runtime_epoch_unix_ns"]
        });
        assert_eq!(fresh["validated_pong_count"], 0);
        assert_eq!(fresh["connection_generation"], 0);
        drop(restarted);
        std::thread::sleep(Duration::from_millis(1100));
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn rejects_relative_symlink_and_untrusted_leaf_without_referent_write() {
        use std::os::unix::fs::{symlink, PermissionsExt};
        let dir = directory();
        assert!(Health::start(PathBuf::from("relative.json")).is_none());
        let sentinel = dir.join("sentinel");
        fs::write(&sentinel, b"secret sentinel").unwrap();
        let link = dir.join("link");
        symlink(&sentinel, &link).unwrap();
        assert!(Health::start(link).is_none());
        assert_eq!(fs::read(&sentinel).unwrap(), b"secret sentinel");
        let alias = dir.join("alias");
        symlink(&dir, &alias).unwrap();
        assert!(Health::start(alias.join("health.json")).is_none());
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o777)).unwrap();
        assert!(Health::start(dir.join("health.json")).is_none());
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).unwrap();
        fs::remove_dir_all(dir).unwrap();
    }
}
