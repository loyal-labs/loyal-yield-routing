ALTER TABLE loyal_yield.solana_week_quest_completions
  ADD CONSTRAINT solana_week_quest_completions_quest_kind_check
  CHECK (quest_kind IN ('earn_deposit', 'first_autodeposit_sweep')) NOT VALID;

ALTER TABLE loyal_yield.solana_week_quest_completions
  ADD CONSTRAINT solana_week_quest_completions_status_check
  CHECK (status IN ('pending', 'reported', 'failed')) NOT VALID;

ALTER TABLE loyal_yield.solana_week_quest_completions
  ADD CONSTRAINT solana_week_quest_completions_solana_status_check
  CHECK (solana_status IS NULL OR solana_status IN ('completed', 'already_completed')) NOT VALID;
