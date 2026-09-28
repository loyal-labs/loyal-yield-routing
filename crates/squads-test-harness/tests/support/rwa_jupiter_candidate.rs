use loyal_actions::{
    SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
};
use serde_json::Value;
use solana_sdk::pubkey::Pubkey;
use std::str::FromStr;

// Parse only the existing four constraint primitives used by these candidate
// groups. Shared by allocation and full deployed-program execution probes.
pub fn constraints(specs: &[Value]) -> Vec<SemanticProgramInteractionConstraint> {
    specs
        .iter()
        .map(|c| SemanticProgramInteractionConstraint {
            program_id: Pubkey::from_str(c["programId"].as_str().unwrap()).unwrap(),
            account_pubkeys: c["accountPubkeys"]
                .as_array()
                .unwrap()
                .iter()
                .map(|a| {
                    (
                        u8::try_from(a["index"].as_u64().unwrap()).unwrap(),
                        a["pubkeys"]
                            .as_array()
                            .unwrap()
                            .iter()
                            .map(|v| Pubkey::from_str(v.as_str().unwrap()).unwrap())
                            .collect(),
                    )
                })
                .collect(),
            account_data: vec![],
            data: c["data"]
                .as_array()
                .unwrap()
                .iter()
                .map(|d| {
                    let offset = d["offset"].as_u64().unwrap();
                    match d["kind"].as_str().unwrap() {
                        "slice-equals" => {
                            let s = d["valueHex"].as_str().unwrap();
                            assert_eq!(s.len() % 2, 0);
                            SemanticProgramInteractionDataConstraint::SliceEquals {
                                offset,
                                value: (0..s.len())
                                    .step_by(2)
                                    .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
                                    .collect(),
                            }
                        }
                        "u64-less-than-or-equal" => {
                            SemanticProgramInteractionDataConstraint::U64LessThanOrEqual {
                                offset,
                                value: d["value"].as_u64().unwrap(),
                            }
                        }
                        "u16-less-than-or-equal" => {
                            SemanticProgramInteractionDataConstraint::U16LessThanOrEqual {
                                offset,
                                value: u16::try_from(d["value"].as_u64().unwrap()).unwrap(),
                            }
                        }
                        "u8-equals" => SemanticProgramInteractionDataConstraint::U8Equals {
                            offset,
                            value: u8::try_from(d["value"].as_u64().unwrap()).unwrap(),
                        },
                        _ => panic!("candidate introduces unsupported constraint semantics"),
                    }
                })
                .collect(),
        })
        .collect()
}
