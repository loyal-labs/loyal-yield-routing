use axum::http::HeaderValue;
use loyal_yield_realtime_core::BoxError;
use url::Url;

#[derive(Clone)]
pub(crate) enum AllowedOrigin {
    Exact(HeaderValue),
    HttpsSubdomains(String),
}

impl AllowedOrigin {
    pub(crate) fn parse(origin: &str, is_render: bool) -> Result<Self, BoxError> {
        if origin.contains('*') {
            // Wildcards are DNS suffix rules, not arbitrary glob expressions.
            let suffix = origin
                .strip_prefix("https://*.")
                .ok_or("wildcard origins must use https://*.domain")?;
            if !suffix.contains('.')
                || !valid_dns_name(suffix)
                || suffix.parse::<std::net::IpAddr>().is_ok()
            {
                return Err(
                    "wildcard origins require a DNS domain suffix without ports or paths".into(),
                );
            }
            return Ok(Self::HttpsSubdomains(format!(".{suffix}")));
        }

        let parsed = Url::parse(origin)?;
        if !matches!(parsed.scheme(), "http" | "https")
            || parsed.path() != "/"
            || parsed.query().is_some()
            || parsed.fragment().is_some()
            || parsed.username() != ""
            || parsed.password().is_some()
        {
            return Err(format!("invalid exact origin {origin}").into());
        }
        let host = parsed.host_str().ok_or("origin host missing")?;
        if is_render && matches!(host, "localhost" | "127.0.0.1" | "::1") {
            return Err("localhost origins are forbidden on Render".into());
        }
        Ok(Self::Exact(HeaderValue::from_str(origin)?))
    }

    pub(crate) fn matches(&self, origin: &HeaderValue) -> bool {
        match self {
            Self::Exact(exact) => exact == origin,
            Self::HttpsSubdomains(suffix) => {
                let Ok(origin) = origin.to_str() else {
                    return false;
                };
                // Browser origins contain no credentials, path or port here.
                // Parsing URL-like input would normalize away some of these.
                let Some(host) = origin.strip_prefix("https://") else {
                    return false;
                };
                valid_dns_name(host) && host.strip_suffix(suffix).is_some_and(valid_dns_name)
            }
        }
    }
}

fn valid_dns_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 253
        && value.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && !label.starts_with('-')
                && !label.ends_with('-')
                && label
                    .bytes()
                    .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wildcard_configuration_rejects_broad_or_ambiguous_patterns() {
        for invalid in [
            "*",
            "https://*",
            "*.preview.askloyal.com",
            "http://*.preview.askloyal.com",
            "https://*.com",
            "https://*.localhost",
            "https://*.127.0.0.1",
            "https://*.preview.askloyal.com:443",
            "https://*.preview.askloyal.com/",
            "https://*.preview.askloyal.com?x=1",
            "https://*.preview.askloyal.com#x",
            "https://*.preview.askloyal.com@evil.com",
            "https://*.*.askloyal.com",
            "https://prefix*.preview.askloyal.com",
            "https://*.-preview.askloyal.com",
        ] {
            assert!(AllowedOrigin::parse(invalid, true).is_err(), "{invalid}");
        }
    }
}
