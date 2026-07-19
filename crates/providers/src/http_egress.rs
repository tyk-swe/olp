//! Shared public-IP classification for outbound network clients.
//!
//! This crate deliberately classifies already-parsed IP addresses only. URL
//! parsing, DNS resolution, connection pinning, and request policy remain the
//! responsibility of each caller.

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

/// Returns whether an IP address is safe to use as a public egress target.
#[must_use]
pub fn is_public_ip(address: IpAddr) -> bool {
    !is_blocked_ip(address)
}

fn is_blocked_ip(address: IpAddr) -> bool {
    match address {
        IpAddr::V4(address) => is_blocked_ipv4(address),
        IpAddr::V6(address) => is_blocked_ipv6(address),
    }
}

fn is_blocked_ipv4(address: Ipv4Addr) -> bool {
    let octets = address.octets();
    address.is_private()
        || address.is_loopback()
        || address.is_link_local()
        || address.is_multicast()
        || address.is_unspecified()
        || address.is_broadcast()
        // Current cloud metadata services use link-local space, including
        // 169.254.169.254. Keep the exact address explicit as defense in depth.
        || address == Ipv4Addr::new(169, 254, 169, 254)
        // "This network", shared carrier space, benchmarking, documentation,
        // and future/reserved ranges are not public upstream destinations.
        || octets[0] == 0
        || (octets[0] == 100 && (64..=127).contains(&octets[1]))
        || (octets[0] == 192 && octets[1] == 0 && octets[2] == 0)
        || (octets[0] == 192 && octets[1] == 0 && octets[2] == 2)
        || (octets[0] == 192 && octets[1] == 31 && octets[2] == 196)
        || (octets[0] == 192 && octets[1] == 52 && octets[2] == 193)
        || (octets[0] == 192 && octets[1] == 88 && octets[2] == 99)
        || (octets[0] == 192 && octets[1] == 175 && octets[2] == 48)
        || (octets[0] == 198 && (18..=19).contains(&octets[1]))
        || (octets[0] == 198 && octets[1] == 51 && octets[2] == 100)
        || (octets[0] == 203 && octets[1] == 0 && octets[2] == 113)
        || octets[0] >= 240
}

fn is_blocked_ipv6(address: Ipv6Addr) -> bool {
    if let Some(mapped) = address.to_ipv4_mapped() {
        return is_blocked_ipv4(mapped);
    }
    if let Some(compatible) = address.to_ipv4() {
        return is_blocked_ipv4(compatible);
    }
    let segments = address.segments();
    address.is_loopback()
        || address.is_unspecified()
        || address.is_multicast()
        || address.is_unique_local()
        || address.is_unicast_link_local()
        // Only allow the IANA global-unicast allocations below. The remainder
        // of 2000::/3 is reserved for future allocation and must not become a
        // fail-open path to an internally routed network.
        || !is_allocated_global_ipv6(segments)
        // Deprecated site-local addresses remain non-public even though they
        // are outside the modern unique-local prefix.
        || (segments[0] & 0xffc0) == 0xfec0
        // Translation, discard-only, transition, benchmarking, ORCHID, and
        // documentation prefixes are not ordinary globally routed endpoints.
        || (segments[0] == 0x0064
            && segments[1] == 0xff9b
            && segments[2..6].iter().all(|part| *part == 0))
        || (segments[0] == 0x0064 && segments[1] == 0xff9b && segments[2] == 1)
        || (segments[0] == 0x0100 && segments[1..4].iter().all(|part| *part == 0))
        || (segments[0] == 0x2001 && segments[1] <= 0x01ff)
        || (segments[0] == 0x2001 && segments[1] == 0x0db8)
        || segments[0] == 0x2002
        || (segments[0] & 0xfff0) == 0x3ff0
        || segments[0] == 0x5f00
}

fn is_allocated_global_ipv6(segments: [u16; 8]) -> bool {
    match segments[0] {
        // IANA global-unicast allocations as of 2026-07. `2001::/23` is
        // deliberately excluded because this egress policy already treats its
        // special-purpose subranges as non-public.
        0x2001 => matches!(
            segments[1],
            0x0200..=0x0fff
                | 0x1200..=0x1fff
                | 0x2000..=0x3fff
                | 0x4000..=0x4dff
                | 0x5000..=0x5fff
                | 0x8000..=0x9fff
                | 0xa000..=0xbfff
        ),
        0x2003 => segments[1] <= 0x3fff,
        0x2400..=0x241f
        | 0x2600..=0x260f
        | 0x2630..=0x263f
        | 0x2800..=0x280f
        | 0x2a00..=0x2a1f
        | 0x2c00..=0x2c0f => true,
        0x2610 | 0x2620 => segments[1] <= 0x01ff,
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ip(value: &str) -> IpAddr {
        value.parse().unwrap()
    }

    #[test]
    fn rejects_private_special_use_and_documentation_ipv4_ranges() {
        for address in [
            "0.1.2.3",
            "10.0.0.1",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "172.16.0.1",
            "192.0.0.1",
            "192.0.2.1",
            "192.31.196.1",
            "192.52.193.1",
            "192.88.99.1",
            "192.168.0.1",
            "192.175.48.1",
            "198.18.0.1",
            "198.19.255.254",
            "198.51.100.1",
            "203.0.113.1",
            "224.0.0.1",
            "240.0.0.1",
            "255.255.255.255",
        ] {
            assert!(!is_public_ip(ip(address)), "{address} must be blocked");
        }
    }

    #[test]
    fn rejects_special_use_ipv6_ranges() {
        for address in [
            "::",
            "::1",
            "fc00::1",
            "fe80::1",
            "fec0::1",
            "ff02::1",
            "64:ff9b::7f00:1",
            "64:ff9b:1::1",
            "100::1",
            "100:0:0:1::1",
            "2001::1",
            "2001:1ff::1",
            "2001:db8::1",
            "2002:7f00:1::1",
            "2d00::1",
            "2e00::1",
            "3000::1",
            "3fff::1",
            "4000::1",
            "fe00::1",
            "5f00::1",
        ] {
            assert!(!is_public_ip(ip(address)), "{address} must be blocked");
        }
    }

    #[test]
    fn applies_ipv4_policy_to_mapped_and_compatible_ipv6_addresses() {
        for address in [
            "::ffff:127.0.0.1",
            "::127.0.0.1",
            "::ffff:192.0.2.1",
            "::192.0.2.1",
        ] {
            assert!(!is_public_ip(ip(address)), "{address} must be blocked");
        }
        for address in ["::ffff:8.8.8.8", "::8.8.8.8"] {
            assert!(is_public_ip(ip(address)), "{address} must be accepted");
        }
    }

    #[test]
    fn accepts_global_addresses() {
        for address in [
            "1.1.1.1",
            "8.8.8.8",
            "2001:4860:4860::8888",
            "2404:6800:4004::200e",
            "2606:4700:4700::1111",
            "2c0f:f248::1",
            "2610:1ff::1",
            "2620:1ff::1",
        ] {
            assert!(is_public_ip(ip(address)), "{address} must be accepted");
        }
    }

    #[test]
    fn rejects_unallocated_gaps_inside_narrow_global_allocations() {
        for address in ["2610:200::1", "2611::1", "2620:200::1", "2621::1"] {
            assert!(!is_public_ip(ip(address)), "{address} must be blocked");
        }
    }
}
