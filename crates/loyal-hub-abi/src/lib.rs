//! Generated Loyal Hub ABI constants and byte layouts.
//!
//! Edit `schema/loyal_hub_abi.schema` when the wire format changes. The
//! generated module computes offsets and lengths from the schema so program,
//! SDK, Squads policy, and verification code do not hand-maintain byte layout.

include!(concat!(env!("OUT_DIR"), "/generated.rs"));

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    #[test]
    fn swap_layout_matches_public_policy_offsets() {
        assert_eq!(SWAP_EXACT_IN_TAG_OFFSET, 0);
        assert_eq!(SWAP_EXACT_IN_MAX_FEE_BPS_DATA_OFFSET, 25);
        assert_eq!(SWAP_EXACT_IN_DATA_LEN, 28);
    }

    #[test]
    fn config_layout_has_expected_capacity() {
        assert_eq!(CONFIG_INIT_FIXED_LEN, config_init::FIXED_LEN);
        assert_eq!(CONFIG_ACCOUNT_MAX_LEN, config_account::MAX_LEN);
        assert_eq!(
            CONFIG_ACCOUNT_MAX_LEN,
            config_account::FIXED_LEN + (MAX_ALLOWED_MINTS * config_account::ALLOWED_MINT_ITEM_LEN),
        );
    }

    #[test]
    fn abi_spec_drift_gate() {
        let schema = parse_abi_schema(include_str!("../schema/loyal_hub_abi.schema"));
        let spec = parse_qedspec(include_str!(
            "../../loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
        ));

        for check in DRIFT_CHECKS {
            let schema_accounts = schema
                .accounts
                .get(check.instruction)
                .unwrap_or_else(|| panic!("missing ABI accounts for {}", check.instruction));
            let spec_handler = spec
                .handlers
                .get(check.handler)
                .unwrap_or_else(|| panic!("missing QEDGen handler {}", check.handler));
            let expected_accounts = normalize_accounts(schema_accounts);
            match check.account_mode {
                AccountMode::Exact => {
                    assert_eq!(
                        spec_handler.accounts, expected_accounts,
                        "{} accounts drifted from ABI schema",
                        check.handler
                    );
                }
                AccountMode::OrderedSubset => {
                    assert_ordered_subset(
                        &spec_handler.accounts,
                        &expected_accounts,
                        &format!("{} accounts", check.handler),
                    );
                }
            }

            let schema_args = schema_args_for_check(&schema, check);
            let expected_args = normalize_args(&schema_args, check);
            match check.arg_mode {
                ArgMode::Exact => {
                    assert_eq!(
                        spec_handler.params, expected_args,
                        "{} params drifted from ABI schema",
                        check.handler
                    );
                }
                ArgMode::OrderedSubset => {
                    assert_ordered_subset(
                        &spec_handler.params,
                        &expected_args,
                        &format!("{} params", check.handler),
                    );
                }
                ArgMode::UnorderedSubset => {
                    for arg in &expected_args {
                        assert!(
                            spec_handler.params.contains(arg),
                            "{} params are missing ABI arg {arg}",
                            check.handler
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn rebalance_batch_model_tracks_abi_stride() {
        let spec = parse_qedspec(include_str!(
            "../../loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
        ));

        for arity in 2..=MAX_REBALANCE_TRANSFERS {
            let handler = format!("rebalance_inventory_{arity}");
            let spec_handler = spec
                .handlers
                .get(&handler)
                .unwrap_or_else(|| panic!("missing QEDGen handler {handler}"));

            assert_eq!(
                spec_handler.params,
                expected_rebalance_batch_params(arity),
                "{handler} params drifted from repeated REBALANCE_TRANSFER shape"
            );
            assert_eq!(
                spec_handler.accounts,
                expected_rebalance_batch_accounts(arity),
                "{handler} accounts drifted from ABI transfer stride"
            );
        }
    }

    const DRIFT_CHECKS: &[DriftCheck] = &[
        DriftCheck {
            instruction: "INITIALIZE_CONFIG",
            handler: "initialize_config",
            arg_record: "CONFIG_INIT",
            account_mode: AccountMode::OrderedSubset,
            arg_mode: ArgMode::OrderedSubset,
            keep_args: &["MAX_FEE_BPS", "LANE_COUNT"],
            aliases: &[],
        },
        DriftCheck {
            instruction: "SET_MAX_FEE",
            handler: "set_max_fee",
            arg_record: "SET_MAX_FEE_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[("max_fee_bps", "new_max_fee_bps")],
        },
        DriftCheck {
            instruction: "SET_PAUSED",
            handler: "set_paused",
            arg_record: "SET_PAUSED_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[("paused", "paused_value")],
        },
        DriftCheck {
            instruction: "SET_ADMIN",
            handler: "set_admin",
            arg_record: "SET_ADMIN_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[],
        },
        DriftCheck {
            instruction: "SET_HUB_AUTHORIZER",
            handler: "set_hub_authorizer",
            arg_record: "SET_HUB_AUTHORIZER_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[],
        },
        DriftCheck {
            instruction: "SET_INVENTORY_REBALANCER",
            handler: "set_inventory_rebalancer",
            arg_record: "SET_INVENTORY_REBALANCER_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[],
        },
        DriftCheck {
            instruction: "SET_LANE_COUNT",
            handler: "set_lane_count",
            arg_record: "SET_LANE_COUNT_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[],
        },
        DriftCheck {
            instruction: "SWAP_EXACT_IN",
            handler: "swap_exact_in",
            arg_record: "SWAP_EXACT_IN_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::OrderedSubset,
            keep_args: &[],
            aliases: &[],
        },
        DriftCheck {
            instruction: "WITHDRAW_INVENTORY",
            handler: "withdraw_inventory",
            arg_record: "WITHDRAW_INVENTORY_ARGS",
            account_mode: AccountMode::Exact,
            arg_mode: ArgMode::Exact,
            keep_args: &[],
            aliases: &[],
        },
        DriftCheck {
            instruction: "REBALANCE_INVENTORY",
            handler: "rebalance_inventory",
            arg_record: "REBALANCE_TRANSFER",
            account_mode: AccountMode::OrderedSubset,
            arg_mode: ArgMode::UnorderedSubset,
            keep_args: &[],
            aliases: &[],
        },
    ];

    #[derive(Clone, Copy)]
    struct DriftCheck {
        instruction: &'static str,
        handler: &'static str,
        arg_record: &'static str,
        account_mode: AccountMode,
        arg_mode: ArgMode,
        keep_args: &'static [&'static str],
        aliases: &'static [(&'static str, &'static str)],
    }

    #[derive(Clone, Copy)]
    enum AccountMode {
        Exact,
        OrderedSubset,
    }

    #[derive(Clone, Copy)]
    enum ArgMode {
        Exact,
        OrderedSubset,
        UnorderedSubset,
    }

    #[derive(Debug, Default)]
    struct AbiSchema {
        accounts: BTreeMap<String, Vec<IndexedName>>,
        records: BTreeMap<String, Vec<String>>,
    }

    #[derive(Debug)]
    struct IndexedName {
        index: u8,
        name: String,
    }

    #[derive(Debug, Default)]
    struct QedSpec {
        handlers: BTreeMap<String, HandlerShape>,
    }

    #[derive(Debug, Default)]
    struct HandlerShape {
        params: Vec<String>,
        accounts: Vec<String>,
    }

    fn parse_abi_schema(source: &str) -> AbiSchema {
        let mut schema = AbiSchema::default();
        let mut current_record: Option<String> = None;

        for raw_line in source.lines() {
            let line = raw_line.split('#').next().unwrap_or("").trim();
            if line.is_empty() {
                continue;
            }
            let parts = line.split_whitespace().collect::<Vec<_>>();
            match parts.as_slice() {
                ["account", instruction, name, index] => {
                    schema
                        .accounts
                        .entry((*instruction).to_owned())
                        .or_default()
                        .push(IndexedName {
                            index: index.parse().expect("valid ABI account index"),
                            name: (*name).to_owned(),
                        });
                }
                ["record", name] => {
                    current_record = Some((*name).to_owned());
                    schema.records.entry((*name).to_owned()).or_default();
                }
                ["field", name, _ty] => {
                    if let Some(record) = &current_record {
                        schema
                            .records
                            .entry(record.clone())
                            .or_default()
                            .push((*name).to_owned());
                    }
                }
                ["repeat", name, _ty, _max_count, _count_field] => {
                    if let Some(record) = &current_record {
                        schema
                            .records
                            .entry(record.clone())
                            .or_default()
                            .push((*name).to_owned());
                    }
                }
                ["end"] => current_record = None,
                _ => {}
            }
        }

        schema
    }

    fn parse_qedspec(source: &str) -> QedSpec {
        let mut spec = QedSpec::default();
        let lines = source.lines().collect::<Vec<_>>();
        let mut index = 0;
        while index < lines.len() {
            let line = lines[index].trim();
            if !line.starts_with("handler ") {
                index += 1;
                continue;
            }

            let handler_name = line
                .trim_start_matches("handler ")
                .split_whitespace()
                .next()
                .expect("handler name")
                .to_owned();
            let mut header = line.to_owned();
            while index + 1 < lines.len() && !lines[index + 1].trim().starts_with("accounts {") {
                index += 1;
                header.push(' ');
                header.push_str(lines[index].trim());
            }

            let params = parse_handler_params(&header);
            while index < lines.len() && !lines[index].trim().starts_with("accounts {") {
                index += 1;
            }

            let mut accounts = Vec::new();
            if index < lines.len() {
                index += 1;
                while index < lines.len() {
                    let account_line = lines[index].trim();
                    if account_line == "}" {
                        break;
                    }
                    if let Some((name, _rest)) = account_line.split_once(':') {
                        accounts.push(name.trim().to_owned());
                    }
                    index += 1;
                }
            }

            spec.handlers
                .insert(handler_name, HandlerShape { params, accounts });
            index += 1;
        }

        spec
    }

    fn parse_handler_params(header: &str) -> Vec<String> {
        let mut params = Vec::new();
        let mut rest = header;
        while let Some(start) = rest.find('(') {
            let after_start = &rest[start + 1..];
            let Some(end) = after_start.find(')') else {
                break;
            };
            let param = &after_start[..end];
            if let Some((name, _ty)) = param.split_once(':') {
                params.push(name.trim().to_owned());
            }
            rest = &after_start[end + 1..];
        }
        params
    }

    fn normalize_accounts(accounts: &[IndexedName]) -> Vec<String> {
        let mut accounts = accounts
            .iter()
            .filter(|account| {
                !matches!(
                    account.name.as_str(),
                    "TRANSFER_ACCOUNTS_START"
                        | "TRANSFER_ACCOUNT_STRIDE"
                        | "SOURCE_AUTHORITY_RELATIVE"
                        | "SOURCE_INVENTORY_RELATIVE"
                        | "DESTINATION_INVENTORY_RELATIVE"
                )
            })
            .collect::<Vec<_>>();
        accounts.sort_by_key(|account| account.index);
        accounts
            .into_iter()
            .map(|account| account.name.to_ascii_lowercase())
            .collect()
    }

    fn schema_args_for_check(schema: &AbiSchema, check: &DriftCheck) -> Vec<String> {
        let mut args = schema
            .records
            .get(check.arg_record)
            .unwrap_or_else(|| panic!("missing ABI record {}", check.arg_record))
            .clone();
        if !check.keep_args.is_empty() {
            args.retain(|arg| check.keep_args.contains(&arg.as_str()));
        }
        args
    }

    fn normalize_args(args: &[String], check: &DriftCheck) -> Vec<String> {
        args.iter()
            .map(|arg| arg.to_ascii_lowercase())
            .map(|arg| {
                check
                    .aliases
                    .iter()
                    .find_map(|(from, to)| (*from == arg).then_some((*to).to_owned()))
                    .unwrap_or(arg)
            })
            .collect()
    }

    fn expected_rebalance_batch_params(arity: usize) -> Vec<String> {
        let mut params = Vec::with_capacity(arity * 3);
        for index in 0..arity {
            params.push(format!("amount_{index}"));
            params.push(format!("from_lane_id_{index}"));
            params.push(format!("to_lane_id_{index}"));
        }
        params
    }

    fn expected_rebalance_batch_accounts(arity: usize) -> Vec<String> {
        let mut accounts = vec![
            "config".to_owned(),
            "inventory_rebalancer".to_owned(),
            "token_program".to_owned(),
            "mint".to_owned(),
        ];
        for index in 0..arity {
            accounts.push(format!("source_authority_{index}"));
            accounts.push(format!("source_inventory_{index}"));
            accounts.push(format!("destination_inventory_{index}"));
        }
        accounts
    }

    fn assert_ordered_subset(actual: &[String], expected: &[String], label: &str) {
        let mut cursor = 0;
        for expected_item in expected {
            let Some(relative_position) = actual[cursor..]
                .iter()
                .position(|actual_item| actual_item == expected_item)
            else {
                panic!("{label} missing ordered ABI item {expected_item}; actual={actual:?}");
            };
            cursor += relative_position + 1;
        }
    }
}
