use loyal_yield_store::fleet_orchestration::MultiplyRouteState;
use serde::Serialize;

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RouteView<'a> {
    pub route_key: &'a str,
    pub vault_id: i64,
    pub generation: u64,
    pub cycle: u64,
    pub goal: String,
    pub current_operation_id: Option<&'a str>,
    pub frontend: &'a loyal_yield_store::fleet_orchestration::MultiplyFrontendView,
}

pub fn route_view(state: &MultiplyRouteState) -> RouteView<'_> {
    RouteView {
        route_key: &state.route_key,
        vault_id: state.vault_id,
        generation: state.generation,
        cycle: state.cycle,
        goal: serde_json::to_value(state.goal)
            .ok()
            .and_then(|value| value.as_str().map(ToOwned::to_owned))
            .unwrap_or_else(|| "invalid".to_owned()),
        current_operation_id: state.current_operation_id.as_deref(),
        frontend: &state.frontend,
    }
}
