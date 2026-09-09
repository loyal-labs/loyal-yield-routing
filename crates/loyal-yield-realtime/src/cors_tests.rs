use super::*;
use axum::{body::Body, http::Request};
use tower::ServiceExt;

// Exercise the real CORS layer with browser preflights, not just its matcher.
#[tokio::test]
async fn preview_preflights_reflect_only_allowed_origins() {
    let rules = [
        "https://askloyal.com",
        "https://www.askloyal.com",
        "https://*.preview.askloyal.com",
    ]
    .into_iter()
    .map(|origin| AllowedOrigin::parse(origin, true).unwrap())
    .collect();
    let app = Router::new()
        .route("/events", get(|| async { StatusCode::UNAUTHORIZED }))
        .layer(cors_layer(
            rules,
            Some(VercelPreviewOriginRule {
                project: "loyal-frontend".into(),
                team: "loyal-team".into(),
            }),
        ));

    for (origin, allowed) in [
        ("https://askloyal.com", true),
        ("https://www.askloyal.com", true),
        (
            "https://ask-2211-move-autodeposit-client-side.preview.askloyal.com",
            true,
        ),
        ("https://nested.branch.preview.askloyal.com", true),
        (
            "https://loyal-frontend-git-test-loyal-team.vercel.app",
            true,
        ),
        ("https://preview.askloyal.com", false),
        ("https://evilpreview.askloyal.com", false),
        ("https://branch.preview.askloyal.com.evil.com", false),
        ("https://branch.preview.askloyal.com@evil.com", false),
        ("https://branch.preview.askloyal.com:444", false),
        ("http://branch.preview.askloyal.com", false),
        ("https://branch.preview.askloyal.com/path", false),
        ("https://branch.preview.askloyal.com?query", false),
        ("https://branch.preview.askloyal.com#fragment", false),
        ("https://.preview.askloyal.com", false),
        ("https://-branch.preview.askloyal.com", false),
        ("https://branch.preview.askloyal.com.", false),
        (
            "https://loyal-frontend-git-test-other-team.vercel.app",
            false,
        ),
        ("null", false),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::builder()
                    .method(Method::OPTIONS)
                    .uri("/events")
                    .header(header::ORIGIN, origin)
                    .header(header::ACCESS_CONTROL_REQUEST_METHOD, "GET")
                    .header(
                        header::ACCESS_CONTROL_REQUEST_HEADERS,
                        "authorization,last-event-id",
                    )
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert!(response.status().is_success());
        assert_eq!(
            response
                .headers()
                .get(header::ACCESS_CONTROL_ALLOW_ORIGIN)
                .map(|value| value.to_str().unwrap()),
            allowed.then_some(origin),
            "{origin}"
        );
        if allowed {
            let headers = response.headers()[header::ACCESS_CONTROL_ALLOW_HEADERS]
                .to_str()
                .unwrap();
            assert!(headers.contains("authorization") && headers.contains("last-event-id"));
            assert!(response.headers()[header::VARY]
                .to_str()
                .unwrap()
                .contains("origin"));
        }
    }

    // CORS admission does not turn an auth failure into a successful response.
    let response = app
        .oneshot(
            Request::builder()
                .uri("/events")
                .header(header::ORIGIN, "https://branch.preview.askloyal.com")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
}
