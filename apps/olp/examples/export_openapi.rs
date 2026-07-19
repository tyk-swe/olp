use olp::management_openapi;

fn main() {
    println!(
        "{}",
        serde_json::to_string_pretty(&management_openapi())
            .expect("OpenAPI document must serialize")
    );
}
