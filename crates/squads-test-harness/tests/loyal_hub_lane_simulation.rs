#[path = "loyal_hub_lane_simulation/support.rs"]
mod support;

use squads_test_harness::{PYUSD_MINT, USDC_MINT};
use support::*;

#[test]
fn thirty_wallets_swap_across_hub_lanes_with_rebalance() {
    let Some(mut simulation) = HubLaneSimulation::setup(DEFAULT_LANE_COUNT, WALLET_COUNT) else {
        return;
    };

    let first_wave = simulation.execute_wave(&wave_one());
    simulation
        .rebalance_during_active_wave(
            &first_wave,
            &[
                PlannedRebalance {
                    mint: USDC_MINT,
                    from_lane_id: 0,
                    to_lane_id: 2,
                    amount: 650_000,
                },
                PlannedRebalance {
                    mint: PYUSD_MINT,
                    from_lane_id: 1,
                    to_lane_id: 3,
                    amount: 600_000,
                },
            ],
        )
        .expect_err("scheduler should reject rebalances that touch active wave lanes");

    let maintenance = simulation.settle_wave(first_wave);
    simulation
        .rebalance_during_maintenance(
            &maintenance,
            &[
                PlannedRebalance {
                    mint: USDC_MINT,
                    from_lane_id: 0,
                    to_lane_id: 2,
                    amount: 650_000,
                },
                PlannedRebalance {
                    mint: PYUSD_MINT,
                    from_lane_id: 1,
                    to_lane_id: 3,
                    amount: 600_000,
                },
            ],
        )
        .expect("maintenance rebalances after the wave settles");

    simulation.execute_wave(&wave_two());
    simulation.assert_all_balances();
    simulation.assert_total_conservation();
}

#[test]
fn simulation_drains_lane_then_planner_refills_from_surplus_lane() {
    let Some(mut simulation) = HubLaneSimulation::setup(DEFAULT_LANE_COUNT, 5) else {
        return;
    };
    let drain_wave = simulation.execute_wave(&drain_lane_wave());
    let maintenance = simulation.settle_wave(drain_wave);

    let oversized_swap = PlannedSwap {
        wallet_index: 3,
        lane_id: 0,
        direction: SwapDirection::PyusdToUsdc,
        amount_in: 1_000_000,
    };
    let error = simulation
        .execute_swap(oversized_swap)
        .expect_err("drained lane should not satisfy an oversized swap");
    assert!(
        error.contains("InsufficientFunds") || error.contains("Custom"),
        "{error}"
    );

    let planner = InventoryPlanner {
        threshold: 1_000_000,
        target: 5_000_000,
        max_transfer_amount: u64::MAX,
    };
    let ledger = simulation.ledger();
    let refills = planner.plan_refill(&ledger, 0, USDC_MINT);
    assert_eq!(
        refills,
        vec![PlannedRebalance {
            mint: USDC_MINT,
            from_lane_id: 1,
            to_lane_id: 0,
            amount: 4_590_400,
        }]
    );

    simulation
        .rebalance_during_maintenance(&maintenance, &refills)
        .expect("planner refills the drained lane from the highest-surplus lane");
    simulation
        .execute_swap(oversized_swap)
        .expect("same lane can fill again after planner rebalance");
    simulation.assert_total_conservation();
}

#[test]
fn simulation_rejects_rebalance_on_active_swap_lane() {
    let active_wave = ActiveWave::from_wave(&SwapWave::new(vec![
        PlannedSwap {
            wallet_index: 0,
            lane_id: 1,
            direction: SwapDirection::UsdcToPyusd,
            amount_in: 100_000,
        },
        PlannedSwap {
            wallet_index: 1,
            lane_id: 3,
            direction: SwapDirection::PyusdToUsdc,
            amount_in: 100_000,
        },
    ]));
    let error = LaneScheduler::ensure_rebalance_avoids_active_lanes(
        active_wave.active_lanes(),
        &[PlannedRebalance {
            mint: USDC_MINT,
            from_lane_id: 0,
            to_lane_id: 3,
            amount: 50_000,
        }],
    )
    .expect_err("scheduler rejects touching a lane used by active swaps");

    assert!(error.contains("active lane"), "{error}");
}

#[test]
fn simulation_scheduler_selects_sufficient_lowest_load_lane() {
    let candidates = [
        LaneCandidate {
            lane_id: 0,
            output_inventory: 2_000_000,
            in_flight_count: 3,
        },
        LaneCandidate {
            lane_id: 1,
            output_inventory: 900_000,
            in_flight_count: 0,
        },
        LaneCandidate {
            lane_id: 15,
            output_inventory: 2_000_000,
            in_flight_count: 1,
        },
        LaneCandidate {
            lane_id: GROWTH_LANE_COUNT - 1,
            output_inventory: 2_000_000,
            in_flight_count: 1,
        },
    ];

    assert_eq!(
        LaneScheduler::choose_swap_lane(&candidates, 1_000_000),
        Some(15)
    );
    assert_eq!(
        LaneScheduler::choose_swap_lane(&candidates, 3_000_000),
        None
    );
}

#[test]
fn simulation_policy_does_not_change_when_lane_count_grows() {
    let Some(mut simulation) = HubLaneSimulation::setup(GROWTH_LANE_COUNT, 1) else {
        return;
    };
    let wallet = &simulation.wallets[0];
    assert_eq!(wallet.swap_action.spec.constraint_count, 1);

    simulation
        .execute_swap(PlannedSwap {
            wallet_index: 0,
            lane_id: GROWTH_LANE_COUNT - 1,
            direction: SwapDirection::UsdcToPyusd,
            amount_in: 500_000,
        })
        .expect("same Loyal Action shape works on a non-default high lane");
    assert_eq!(simulation.wallets[0].swap_action.spec.constraint_count, 1);
    simulation.assert_all_balances();
}

#[test]
fn simulation_records_lane_metrics() {
    let Some(mut simulation) = HubLaneSimulation::setup(2, 2) else {
        return;
    };
    let maintenance = MaintenanceWindow;

    simulation
        .execute_swap(PlannedSwap {
            wallet_index: 0,
            lane_id: 0,
            direction: SwapDirection::UsdcToPyusd,
            amount_in: 1_000_000,
        })
        .expect("recorded swap succeeds");
    simulation
        .execute_swap(PlannedSwap {
            wallet_index: 1,
            lane_id: 0,
            direction: SwapDirection::PyusdToUsdc,
            amount_in: 20_000_000,
        })
        .expect_err("failed swap is counted for the lane");
    simulation
        .rebalance_during_maintenance(
            &maintenance,
            &[PlannedRebalance {
                mint: USDC_MINT,
                from_lane_id: 1,
                to_lane_id: 0,
                amount: 250_000,
            }],
        )
        .expect("recorded rebalance succeeds");

    let lane_zero = simulation.metrics(0);
    assert_eq!(lane_zero.inflow.usdc, 1_000_000);
    assert_eq!(lane_zero.outflow.pyusd, 999_000);
    assert_eq!(lane_zero.minimum_inventory.pyusd, 9_001_000);
    assert_eq!(lane_zero.failed_swap_count, 1);
    assert_eq!(lane_zero.rebalance_volume.usdc, 250_000);

    let lane_one = simulation.metrics(1);
    assert_eq!(lane_one.minimum_inventory.usdc, 9_750_000);
    assert_eq!(lane_one.rebalance_volume.usdc, 250_000);
}

#[test]
fn simulation_planner_caps_refill_transfers() {
    let Some(mut simulation) = HubLaneSimulation::setup(DEFAULT_LANE_COUNT, 3) else {
        return;
    };
    simulation.execute_wave(&drain_lane_wave());

    let planner = InventoryPlanner {
        threshold: 1_000_000,
        target: 5_000_000,
        max_transfer_amount: 1_000_000,
    };
    let ledger = simulation.ledger();
    let refills = planner.plan_refill(&ledger, 0, USDC_MINT);

    assert_eq!(
        refills,
        vec![PlannedRebalance {
            mint: USDC_MINT,
            from_lane_id: 1,
            to_lane_id: 0,
            amount: 1_000_000,
        }]
    );
}

#[test]
fn simulation_derives_ledger_and_metrics_from_events() {
    let Some(mut simulation) = HubLaneSimulation::setup(2, 2) else {
        return;
    };

    simulation
        .execute_swap(PlannedSwap {
            wallet_index: 0,
            lane_id: 0,
            direction: SwapDirection::UsdcToPyusd,
            amount_in: 1_000_000,
        })
        .expect("accepted swap records an event");
    simulation
        .execute_swap(PlannedSwap {
            wallet_index: 1,
            lane_id: 0,
            direction: SwapDirection::PyusdToUsdc,
            amount_in: 20_000_000,
        })
        .expect_err("rejected swap records an event without moving balances");
    simulation
        .rebalance_during_maintenance(
            &MaintenanceWindow,
            &[PlannedRebalance {
                mint: USDC_MINT,
                from_lane_id: 1,
                to_lane_id: 0,
                amount: 250_000,
            }],
        )
        .expect("accepted rebalance records an event");

    assert_eq!(simulation.events().len(), 3);
    assert!(matches!(
        simulation.events()[0],
        SimulationEvent::SwapAccepted { .. }
    ));
    assert!(matches!(
        simulation.events()[1],
        SimulationEvent::SwapRejected { .. }
    ));
    assert!(matches!(
        simulation.events()[2],
        SimulationEvent::RebalanceAccepted { .. }
    ));

    let ledger = simulation.ledger();
    assert_eq!(ledger.lane_amount(0, USDC_MINT), 11_250_000);
    assert_eq!(ledger.lane_amount(0, PYUSD_MINT), 9_001_000);
    assert_eq!(ledger.lane_amount(1, USDC_MINT), 9_750_000);
    assert_eq!(ledger.wallet_amount(0, USDC_MINT), 3_000_000);
    assert_eq!(ledger.wallet_amount(0, PYUSD_MINT), 4_999_000);

    let lane_zero = simulation.metrics(0);
    assert_eq!(lane_zero.failed_swap_count, 1);
    assert_eq!(lane_zero.rebalance_volume.usdc, 250_000);
}

fn drain_lane_wave() -> SwapWave {
    SwapWave::new(vec![
        PlannedSwap {
            wallet_index: 0,
            lane_id: 0,
            direction: SwapDirection::PyusdToUsdc,
            amount_in: 3_200_000,
        },
        PlannedSwap {
            wallet_index: 1,
            lane_id: 0,
            direction: SwapDirection::PyusdToUsdc,
            amount_in: 3_200_000,
        },
        PlannedSwap {
            wallet_index: 2,
            lane_id: 0,
            direction: SwapDirection::PyusdToUsdc,
            amount_in: 3_200_000,
        },
    ])
}

fn wave_one() -> SwapWave {
    SwapWave::new(
        (0..WALLET_COUNT)
            .map(|wallet_index| PlannedSwap {
                wallet_index,
                lane_id: (wallet_index % usize::from(DEFAULT_LANE_COUNT)) as u8,
                direction: if wallet_index % 2 == 0 {
                    SwapDirection::UsdcToPyusd
                } else {
                    SwapDirection::PyusdToUsdc
                },
                amount_in: 300_000 + ((wallet_index % 3) as u64 * 50_000),
            })
            .collect(),
    )
}

fn wave_two() -> SwapWave {
    SwapWave::new(
        (0..12)
            .map(|offset| PlannedSwap {
                wallet_index: offset,
                lane_id: if offset < 8 { 2 } else { 3 },
                direction: if offset % 3 == 0 {
                    SwapDirection::PyusdToUsdc
                } else {
                    SwapDirection::UsdcToPyusd
                },
                amount_in: 250_000,
            })
            .collect(),
    )
}
