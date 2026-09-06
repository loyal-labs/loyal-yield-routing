//! Existing Go bridge compiler plus deployed programs, in the same OnRe SVM.
//! Only the Clock advances; no custody, receipt or ticket reset is permitted.
use super::*;
use std::process::Command;

pub const IDLE: &str = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh";
pub const STAGED: &str = "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M";
const CASH: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
const TICKET: &str = "C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5";

pub fn execute(
    svm: &mut LiteSVM,
    plan: &Value,
    addresses: &[Pubkey],
    action: &str,
    raw: u64,
) -> Value {
    assert_eq!(
        lending_position(
            svm,
            key(plan["onreLending"]["obligation"].as_str().unwrap())
        ),
        (0, 0)
    );
    assert_eq!(
        amount(svm, key(plan["collateralCustody"].as_str().unwrap())),
        0
    );
    let before = capture(svm, addresses);
    let mut clock = svm.get_sysvar::<Clock>();
    let old_slot = clock.slot;
    clock.slot = clock.slot.checked_add(1).unwrap();
    // No elapsed wall-time/interest claim: only the local observation slot advances.
    svm.set_sysvar(&clock);
    let execution_before = capture(svm, addresses);
    let input = json!({"action":action,"amountRaw":raw,"slot":clock.slot,"accounts":capture(svm,&[key(IDLE),key(STAGED),key(CASH)])});
    let binary = std::env::var("PHASE3_ONRE_BRIDGE_COMPILER").unwrap();
    assert_eq!(binary, "/private/tmp/phase3-onre-bridge-test");
    let output = Command::new(binary)
        .arg("-test.run=^TestExportOnReBridgeProbe$")
        .env(
            "PHASE3_ONRE_BRIDGE_INPUT",
            serde_json::to_string(&input).unwrap(),
        )
        .output()
        .unwrap();
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(
        output.status.success(),
        "local Go bridge construction failed: {stdout}"
    );
    let compiled: Value = serde_json::from_str(
        stdout
            .lines()
            .find_map(|l| l.strip_prefix("ONRE_BRIDGE_JSON="))
            .unwrap(),
    )
    .unwrap();
    assert_eq!(compiled["broadcast"], false);
    assert_eq!(compiled["signatureProof"], false);
    assert_eq!(compiled["discoveryOnly"], false);
    assert_eq!(compiled["policies"], plan["onreBridge"]["policies"]);
    let step = &compiled["steps"][0];
    assert_eq!(compiled["steps"].as_array().unwrap().len(), 1);
    let wire = bytes(step, "wireBase64");
    assert!(wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0));
    assert_eq!(sha(&wire), step["wireSha256"]);
    let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
    let balances_before = [
        amount(svm, key(IDLE)),
        amount(svm, key(STAGED)),
        amount(svm, key(CASH)),
    ];
    let result = svm.send_transaction(tx);
    let (meta, error) = match result {
        Ok(m) => (m, None),
        Err(e) => (e.meta, Some(format!("{:?}", e.err))),
    };
    let balances_after = [
        amount(svm, key(IDLE)),
        amount(svm, key(STAGED)),
        amount(svm, key(CASH)),
    ];
    let mut expected = balances_before;
    match action {
        "VOLTR_ALLOCATE_TO_SQUADS" => {
            expected[0] -= raw;
            expected[2] += raw;
        }
        "STAGE_SQUADS_TO_VOLTR" => {
            expected[2] -= raw;
            expected[1] += raw;
        }
        "VOLTR_RESTORE_IDLE" => {
            expected[1] -= raw;
            expected[0] += raw;
        }
        "REPORT_NAV" => assert_eq!(raw, 0),
        _ => panic!("unknown local bridge action"),
    }
    let ticket = svm.get_account(&key(TICKET)).unwrap();
    let ticket_consumed = action == "STAGE_SQUADS_TO_VOLTR"
        || (ticket.data.len() == 96
            && ticket.data[10] == 0
            && u64::from_le_bytes(ticket.data[48..56].try_into().unwrap()) == clock.slot
            && ticket.data[56..96].iter().all(|b| *b == 0));
    let pass = error.is_none()
        && balances_after == expected
        && ticket_consumed
        && meta.compute_units_consumed <= 200000;
    json!({"action":action,"amountRaw":raw,"before":before,"executionBefore":execution_before,"after":capture(svm,addresses),"clockAdvance":{"fromSlot":old_slot,"toSlot":clock.slot,"unixTimestampUnchanged":true},"compilerInput":input,"compiled":compiled,"custodyBefore":balances_before,"custodyAfter":balances_after,"error":error,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"ticketConsumed":ticket_consumed,"pass":pass})
}
