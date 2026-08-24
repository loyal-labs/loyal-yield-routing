use loyal_yield_store::fleet_orchestration::MultiplyRouteState;
use serde::Serialize;

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RouteView<'a> {
    pub route_key: &'a str,
    pub settings: &'a str,
    pub vault_index: u8,
    pub vault: &'a str,
    pub generation: u64,
    pub cycle: u64,
    pub goal: String,
    pub current_operation_id: Option<&'a str>,
}

pub fn route_view(state: &MultiplyRouteState) -> RouteView<'_> {
    RouteView {
        route_key: &state.route_key,
        settings: &state.settings,
        vault_index: state.vault_index,
        vault: &state.vault,
        generation: state.generation,
        cycle: state.cycle,
        goal: serde_json::to_value(state.goal)
            .ok()
            .and_then(|value| value.as_str().map(ToOwned::to_owned))
            .unwrap_or_else(|| "invalid".to_owned()),
        current_operation_id: state.current_operation_id.as_deref(),
    }
}
