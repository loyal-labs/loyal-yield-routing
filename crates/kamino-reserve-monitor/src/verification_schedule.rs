use std::collections::{BTreeSet, HashMap};

use solana_sdk::pubkey::Pubkey;

#[derive(Clone, Debug)]
pub struct VerificationBatch {
    generations: HashMap<Pubkey, u64>,
}

impl VerificationBatch {
    pub fn reserves(&self) -> Vec<Pubkey> {
        self.generations.keys().copied().collect()
    }
}

#[derive(Debug, Default)]
pub struct DirtyReserveVerificationSchedule {
    generations: HashMap<Pubkey, u64>,
    pending: BTreeSet<Pubkey>,
    in_flight: bool,
}

impl DirtyReserveVerificationSchedule {
    pub fn mark_dirty(&mut self, reserve: Pubkey) {
        let generation = self.generations.entry(reserve).or_default();
        *generation = generation.saturating_add(1);
        self.pending.insert(reserve);
    }

    pub fn request_safety_sweep(&mut self, reserves: impl IntoIterator<Item = Pubkey>) {
        for reserve in reserves {
            self.generations.entry(reserve).or_default();
            self.pending.insert(reserve);
        }
    }

    pub fn begin_batch(&mut self) -> Option<VerificationBatch> {
        if self.in_flight || self.pending.is_empty() {
            return None;
        }
        let generations = std::mem::take(&mut self.pending)
            .into_iter()
            .map(|reserve| {
                let generation = self.generations.get(&reserve).copied().unwrap_or_default();
                (reserve, generation)
            })
            .collect();
        self.in_flight = true;
        Some(VerificationBatch { generations })
    }

    pub fn complete_success(&mut self, batch: &VerificationBatch) -> BTreeSet<Pubkey> {
        self.in_flight = false;
        batch
            .generations
            .iter()
            .filter_map(|(reserve, requested_generation)| {
                let current_generation = self.generations.get(reserve).copied().unwrap_or_default();
                if current_generation != *requested_generation {
                    return None;
                }
                self.pending.remove(reserve);
                Some(*reserve)
            })
            .collect()
    }

    pub fn complete_failure(&mut self, batch: &VerificationBatch) {
        self.in_flight = false;
        self.pending.extend(batch.generations.keys().copied());
    }

    pub fn pending_count(&self) -> usize {
        self.pending.len()
    }

    pub fn in_flight(&self) -> bool {
        self.in_flight
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn reserve(byte: u8) -> Pubkey {
        Pubkey::new_from_array([byte; 32])
    }

    #[test]
    fn repeated_updates_coalesce_into_one_latest_batch_entry() {
        let mut schedule = DirtyReserveVerificationSchedule::default();
        let reserve = reserve(1);

        schedule.mark_dirty(reserve);
        schedule.mark_dirty(reserve);
        schedule.mark_dirty(reserve);

        let batch = schedule
            .begin_batch()
            .expect("dirty reserve should start a batch");
        assert_eq!(batch.reserves(), vec![reserve]);
        assert!(schedule.in_flight());
        assert!(schedule.begin_batch().is_none());
    }

    #[test]
    fn update_during_in_flight_batch_rejects_stale_result_and_stays_pending() {
        let mut schedule = DirtyReserveVerificationSchedule::default();
        let reserve = reserve(2);
        schedule.mark_dirty(reserve);
        let stale_batch = schedule.begin_batch().expect("first batch");

        schedule.mark_dirty(reserve);
        let accepted = schedule.complete_success(&stale_batch);

        assert!(accepted.is_empty());
        assert_eq!(schedule.pending_count(), 1);
        assert_eq!(
            schedule
                .begin_batch()
                .expect("new generation batch")
                .reserves(),
            vec![reserve]
        );
    }

    #[test]
    fn successful_current_result_clears_matching_pending_sweep() {
        let mut schedule = DirtyReserveVerificationSchedule::default();
        let reserve = reserve(3);
        schedule.mark_dirty(reserve);
        let batch = schedule.begin_batch().expect("first batch");
        schedule.request_safety_sweep([reserve]);

        assert_eq!(schedule.complete_success(&batch), BTreeSet::from([reserve]));
        assert_eq!(schedule.pending_count(), 0);
    }

    #[test]
    fn failed_batch_is_retried_without_growing_one_entry_per_failure() {
        let mut schedule = DirtyReserveVerificationSchedule::default();
        let reserve = reserve(4);
        schedule.mark_dirty(reserve);
        let batch = schedule.begin_batch().expect("first batch");

        schedule.complete_failure(&batch);
        schedule.complete_failure(&batch);

        assert_eq!(schedule.pending_count(), 1);
        assert_eq!(
            schedule.begin_batch().expect("retry batch").reserves(),
            vec![reserve]
        );
    }
}
