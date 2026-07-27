//! Bounded exponential backoff with jitter, tracked per dependency.

use std::{
    collections::hash_map::RandomState,
    hash::{BuildHasher, Hasher},
    time::Duration,
};

/// Default first retry delay, matching the durable listener reconnect policy.
pub const DEFAULT_INITIAL_BACKOFF: Duration = Duration::from_millis(250);

/// Default retry ceiling, matching the durable listener reconnect policy.
///
/// Production measurement showed Render's own restart backoff reaching 185
/// seconds during a sustained outage. Capping in-process retries here is the
/// entire point of supervising: recovery latency stops growing without bound.
pub const DEFAULT_MAXIMUM_BACKOFF: Duration = Duration::from_secs(5);

/// A bounded doubling backoff schedule.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BackoffPolicy {
    initial: Duration,
    maximum: Duration,
}

impl BackoffPolicy {
    /// Creates a policy, clamping a maximum below the initial delay up to it.
    pub const fn new(initial: Duration, maximum: Duration) -> Self {
        let initial = if initial.is_zero() {
            DEFAULT_INITIAL_BACKOFF
        } else {
            initial
        };
        Self {
            initial,
            maximum: if maximum.as_nanos() < initial.as_nanos() {
                initial
            } else {
                maximum
            },
        }
    }

    /// Returns the first delay in the schedule.
    pub const fn initial(self) -> Duration {
        self.initial
    }

    /// Returns the delay ceiling.
    pub const fn maximum(self) -> Duration {
        self.maximum
    }

    /// Returns the undelayed schedule value after `consecutive_failures`.
    ///
    /// The first failure yields [`Self::initial`]; each further failure doubles
    /// until [`Self::maximum`].
    pub fn delay_after(self, consecutive_failures: u32) -> Duration {
        let steps = consecutive_failures.saturating_sub(1).min(32);
        self.initial
            .saturating_mul(2_u32.saturating_pow(steps))
            .min(self.maximum)
    }
}

impl Default for BackoffPolicy {
    fn default() -> Self {
        Self::new(DEFAULT_INITIAL_BACKOFF, DEFAULT_MAXIMUM_BACKOFF)
    }
}

/// Applies decorrelated jitter within `[delay / 2, delay]`.
///
/// Six workers restarted within six seconds of each other during the measured
/// outage waves. Jitter keeps their retries from re-synchronizing into the same
/// thundering herd against a dependency that is still recovering.
pub fn jittered(delay: Duration, jitter: &mut Jitter) -> Duration {
    let nanos = u64::try_from(delay.as_nanos()).unwrap_or(u64::MAX);
    if nanos == 0 {
        return delay;
    }
    let half = nanos / 2;
    let span = nanos - half;
    Duration::from_nanos(half.saturating_add(jitter.next_below(span.saturating_add(1))))
}

/// A small deterministic-per-process pseudorandom source.
///
/// Seeded from the OS-seeded hasher state so no random-number dependency is
/// pulled into the worker binaries for what is only retry smearing.
#[derive(Debug)]
pub struct Jitter {
    state: u64,
}

impl Jitter {
    /// Seeds a source from process-local entropy.
    pub fn new() -> Self {
        let mut hasher = RandomState::new().build_hasher();
        hasher.write_u64(std::process::id().into());
        Self {
            state: hasher.finish() | 1,
        }
    }

    fn next_u64(&mut self) -> u64 {
        // xorshift64*, sufficient for smearing retry timing.
        self.state ^= self.state >> 12;
        self.state ^= self.state << 25;
        self.state ^= self.state >> 27;
        self.state.wrapping_mul(0x2545_f491_4f6c_dd1d)
    }

    fn next_below(&mut self, bound: u64) -> u64 {
        if bound <= 1 {
            0
        } else {
            self.next_u64() % bound
        }
    }
}

impl Default for Jitter {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The schedule must double and then stop, because an unbounded schedule is
    /// the defect this crate exists to remove.
    #[test]
    fn schedule_doubles_then_saturates_at_the_ceiling() {
        let policy = BackoffPolicy::default();
        let delays = (1..=8).map(|n| policy.delay_after(n)).collect::<Vec<_>>();
        assert_eq!(
            delays,
            [250, 500, 1_000, 2_000, 4_000, 5_000, 5_000, 5_000].map(Duration::from_millis)
        );
    }

    /// A zero delay would busy-loop and a delay above the ceiling would
    /// reintroduce unbounded recovery latency.
    #[test]
    fn jitter_stays_within_half_the_delay_and_never_reaches_zero() {
        let mut jitter = Jitter::new();
        let delay = Duration::from_millis(4_000);
        for _ in 0..1_000 {
            let jittered = jittered(delay, &mut jitter);
            assert!(jittered >= delay / 2, "{jittered:?} fell below half");
            assert!(jittered <= delay, "{jittered:?} exceeded the delay");
        }
    }

    /// A maximum below the initial delay is a configuration mistake that must
    /// not produce a shrinking schedule.
    #[test]
    fn maximum_below_initial_clamps_to_a_flat_schedule() {
        let policy = BackoffPolicy::new(Duration::from_secs(2), Duration::from_millis(100));
        assert_eq!(policy.maximum(), Duration::from_secs(2));
        assert_eq!(policy.delay_after(5), Duration::from_secs(2));
    }
}
