//! Reads Go golden requests on stdin and prints the retained worker's route
//! for each, or its refusal.
use std::io::Read;

use serde_json::{json, Value};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input)?;
    let cases: Vec<Value> = serde_json::from_str(&input)?;
    let mut out = Vec::with_capacity(cases.len());
    for mut case in cases {
        let request = &case["request"];
        std::env::set_var("POLICY_KEYPAIR", request["policyKeypair"].as_str().ok_or("policyKeypair missing")?);
        match loyal_fleet_worker::klend_golden::same_mint_route(request) {
            Ok(route) => case["output"] = route,
            Err(error) => case["error"] = json!(error.to_string()),
        }
        out.push(case);
    }
    println!("{}", serde_json::to_string_pretty(&out)?);
    Ok(())
}
