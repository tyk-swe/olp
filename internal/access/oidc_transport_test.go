package access

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestOIDCDialTriesValidatedAddresses(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("1.1.1.1")}
	want := []string{"8.8.8.8:443", "[2606:4700:4700::1111]:443", "1.1.1.1:443"}
	for _, tc := range []struct {
		name    string
		success int
	}{
		{"first_address", 1},
		{"second_address", 2},
		{"last_address", 3},
		{"all_fail", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			unreachable := errors.New("unreachable")
			var attempts []string
			conn, err := oidcDialAddresses(t.Context(), "tcp", "443", addresses, func(_ context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" {
					t.Fatalf("dial network=%q, want tcp", network)
				}
				attempts = append(attempts, address)
				if len(attempts) == tc.success {
					return client, nil
				}
				return nil, unreachable
			})
			count := tc.success
			if tc.success == 0 {
				count = len(addresses)
				if conn != nil || !errors.Is(err, unreachable) {
					t.Fatalf("all addresses failed: conn=%v, err=%v", conn, err)
				}
			} else if conn != client || err != nil {
				t.Fatalf("available address was not used: conn=%v, err=%v", conn, err)
			}
			if !slices.Equal(attempts, want[:count]) {
				t.Fatalf("dialed %v, want only the pinned addresses %v", attempts, want[:count])
			}
		})
	}
}

func TestOIDCDialSharesDeadlineAcrossAddresses(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parentTimeout time.Duration
		allFail       bool
	}{
		{"fallback_after_timeout", 0, false},
		{"all_addresses_time_out", 0, true},
		{"shorter_parent_fallback", 600 * time.Millisecond, false},
		{"shorter_parent_expires", 600 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				budget := 3 * time.Second
				if tc.parentTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.parentTimeout)
					defer cancel()
					budget = tc.parentTimeout
				}
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()
				addresses := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}
				attempts := 0
				start := time.Now()
				conn, err := oidcDialAddresses(ctx, "tcp", "443", addresses, func(ctx context.Context, _, _ string) (net.Conn, error) {
					attempts++
					if attempts == 2 && !tc.allFail {
						return client, nil
					}
					<-ctx.Done()
					return nil, ctx.Err()
				})
				elapsed := budget / 2
				if tc.allFail {
					elapsed = budget
					if conn != nil || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("deadline: conn=%v, err=%v", conn, err)
					}
				} else if conn != client || err != nil {
					t.Fatalf("fallback after timeout: conn=%v, err=%v", conn, err)
				}
				if attempts != 2 || time.Since(start) != elapsed {
					t.Fatalf("attempts=%d, elapsed=%v; want 2 attempts in %v", attempts, time.Since(start), elapsed)
				}
			})
		})
	}
}

func TestOIDCDialStopsWhenCanceled(t *testing.T) {
	for _, beforeDial := range []bool{true, false} {
		ctx, cancel := context.WithCancel(t.Context())
		if beforeDial {
			cancel()
		}
		attempts := 0
		addresses := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}
		conn, err := oidcDialAddresses(ctx, "tcp", "443", addresses, func(context.Context, string, string) (net.Conn, error) {
			attempts++
			cancel()
			return nil, context.Canceled
		})
		cancel()
		want := 1
		if beforeDial {
			want = 0
		}
		if conn != nil || !errors.Is(err, context.Canceled) || attempts != want {
			t.Fatalf("cancellation before dial=%v: conn=%v, err=%v, attempts=%d; want %d attempts", beforeDial, conn, err, attempts, want)
		}
	}
}
