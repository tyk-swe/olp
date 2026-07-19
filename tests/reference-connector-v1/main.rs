#![forbid(unsafe_code)]

// This dependency-free fixture is a compile-time shape proof for the frozen
// Connector v1 contract. It is not a gRPC implementation or published SDK.

const CONNECTOR_V1_PROTO_SHA256: &str =
    "d932be2980825922f5f512f7c4babfefb0c4ac6de1501e3d8b96d5a5cfb5b102";
const RPC_METHODS: [&str; 8] = [
    "Handshake",
    "Configure",
    "Deconfigure",
    "DiscoverModels",
    "CertifyCapability",
    "CheckHealth",
    "Execute",
    "Cancel",
];

#[derive(Clone, Copy)]
struct Request(&'static str);

struct Response(&'static str);

trait ConnectorV1Surface {
    fn type_id(&self) -> &str;
    fn handshake(&mut self, request: Request) -> Result<Response, String>;
    fn configure(&mut self, request: Request) -> Result<Response, String>;
    fn deconfigure(&mut self, request: Request) -> Result<Response, String>;
    fn discover_models(&mut self, request: Request) -> Result<Vec<Response>, String>;
    fn certify_capability(&mut self, request: Request) -> Result<Response, String>;
    fn check_health(&mut self, request: Request) -> Result<Response, String>;
    fn execute(&mut self, client_frames: &[Request]) -> Result<Vec<Response>, String>;
    fn cancel(&mut self, request: Request) -> Result<Response, String>;
}

struct ExternalReferenceConnector {
    type_id: String,
}

impl ExternalReferenceConnector {
    fn new(type_id: String) -> Result<Self, String> {
        if !is_dns_style_type_id(&type_id) {
            return Err("fixture type_id is not DNS-style".to_owned());
        }
        Ok(Self { type_id })
    }

    fn respond(expected: &'static str, request: Request) -> Result<Response, String> {
        if request.0 != expected {
            return Err(format!("expected {expected}, received {}", request.0));
        }
        Ok(Response(expected))
    }
}

impl ConnectorV1Surface for ExternalReferenceConnector {
    fn type_id(&self) -> &str {
        &self.type_id
    }

    fn handshake(&mut self, request: Request) -> Result<Response, String> {
        Self::respond("Handshake", request)
    }

    fn configure(&mut self, request: Request) -> Result<Response, String> {
        Self::respond("Configure", request)
    }

    fn deconfigure(&mut self, request: Request) -> Result<Response, String> {
        Self::respond("Deconfigure", request)
    }

    fn discover_models(&mut self, request: Request) -> Result<Vec<Response>, String> {
        Ok(vec![Self::respond("DiscoverModels", request)?])
    }

    fn certify_capability(&mut self, request: Request) -> Result<Response, String> {
        Self::respond("CertifyCapability", request)
    }

    fn check_health(&mut self, request: Request) -> Result<Response, String> {
        Self::respond("CheckHealth", request)
    }

    fn execute(&mut self, client_frames: &[Request]) -> Result<Vec<Response>, String> {
        if client_frames.len() != 1 {
            return Err("fixture expects one Execute frame".to_owned());
        }
        Ok(vec![Self::respond("Execute", client_frames[0])?])
    }

    fn cancel(&mut self, request: Request) -> Result<Response, String> {
        Self::respond("Cancel", request)
    }
}

fn is_dns_style_type_id(value: &str) -> bool {
    if !(3..=128).contains(&value.len()) || !value.contains('.') {
        return false;
    }
    value.split('.').all(|label| {
        !label.is_empty()
            && label.len() <= 63
            && !label.starts_with('-')
            && !label.ends_with('-')
            && label
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
    })
}

fn assert_surface<T: ConnectorV1Surface>() {}

fn invoke_every_rpc(connector: &mut impl ConnectorV1Surface) -> Result<Vec<&'static str>, String> {
    let mut invoked = Vec::with_capacity(RPC_METHODS.len());
    invoked.push(connector.handshake(Request("Handshake"))?.0);
    invoked.push(connector.configure(Request("Configure"))?.0);
    invoked.push(connector.deconfigure(Request("Deconfigure"))?.0);
    invoked.extend(
        connector
            .discover_models(Request("DiscoverModels"))?
            .into_iter()
            .map(|response| response.0),
    );
    invoked.push(
        connector
            .certify_capability(Request("CertifyCapability"))?
            .0,
    );
    invoked.push(connector.check_health(Request("CheckHealth"))?.0);
    invoked.extend(
        connector
            .execute(&[Request("Execute")])?
            .into_iter()
            .map(|response| response.0),
    );
    invoked.push(connector.cancel(Request("Cancel"))?.0);
    Ok(invoked)
}

fn main() -> Result<(), String> {
    assert_surface::<ExternalReferenceConnector>();

    let mut connector = ExternalReferenceConnector::new("com.example.external-fixture".to_owned())?;
    let invoked = invoke_every_rpc(&mut connector)?;
    if invoked.as_slice() != RPC_METHODS {
        return Err("fixture did not invoke the complete frozen RPC surface".to_owned());
    }

    println!("proto_sha256={CONNECTOR_V1_PROTO_SHA256}");
    println!("type_id={}", connector.type_id());
    for method in invoked {
        println!("rpc={method}");
    }
    Ok(())
}
