#!/usr/bin/env perl

use strict;
use warnings;

my $sql = do {
    local $/;
    <>;
};
$sql = q{} if !defined $sql;
my $length = length $sql;
my $offset = 0;
my $normalized = q{};

while ($offset < $length) {
    my $pair = substr $sql, $offset, 2;
    my $character = substr $sql, $offset, 1;

    if ($pair eq q{--}) {
        $normalized .= q{ };
        $offset += 2;
        $offset++ while $offset < $length && substr($sql, $offset, 1) ne "\n";
        next;
    }

    if ($pair eq q{/*}) {
        my $depth = 1;
        $normalized .= q{ };
        $offset += 2;
        while ($offset < $length && $depth > 0) {
            my $nested_pair = substr $sql, $offset, 2;
            if ($nested_pair eq q{/*}) {
                $depth++;
                $offset += 2;
            } elsif ($nested_pair eq q{*/}) {
                $depth--;
                $offset += 2;
            } else {
                $normalized .= "\n" if substr($sql, $offset, 1) eq "\n";
                $offset++;
            }
        }
        die "unmatched SQL block-comment delimiter\n" if $depth != 0;
        next;
    }

    if ($character eq q{'}) {
        my $escape_string = $offset > 0
          && substr($sql, $offset - 1, 1) =~ /[Ee]/
          && ($offset < 2 || substr($sql, $offset - 2, 1) !~ /[A-Za-z0-9_\$]/);
        $normalized .= $character;
        $offset++;
        my $closed = 0;
        while ($offset < $length) {
            my $quoted_character = substr $sql, $offset, 1;
            $normalized .= $quoted_character;
            $offset++;
            if ($escape_string && $quoted_character eq chr(92) && $offset < $length) {
                $normalized .= substr $sql, $offset, 1;
                $offset++;
            } elsif ($quoted_character eq q{'}) {
                if ($offset < $length && substr($sql, $offset, 1) eq q{'}) {
                    $normalized .= q{'};
                    $offset++;
                } else {
                    $closed = 1;
                    last;
                }
            }
        }
        die "unterminated SQL string literal\n" if !$closed;
        next;
    }

    if ($character eq q{"}) {
        $normalized .= $character;
        $offset++;
        my $closed = 0;
        while ($offset < $length) {
            my $quoted_character = substr $sql, $offset, 1;
            $normalized .= $quoted_character;
            $offset++;
            if ($quoted_character eq q{"}) {
                if ($offset < $length && substr($sql, $offset, 1) eq q{"}) {
                    $normalized .= q{"};
                    $offset++;
                } else {
                    $closed = 1;
                    last;
                }
            }
        }
        die "unterminated SQL quoted identifier\n" if !$closed;
        next;
    }

    if ($character eq q{$}) {
        my $remainder = substr $sql, $offset;
        if ($remainder =~ /\A(\$(?:[A-Za-z_][A-Za-z0-9_]*)?\$)/) {
            my $delimiter = $1;
            my $content_start = $offset + length $delimiter;
            my $content_end = index $sql, $delimiter, $content_start;
            die "unterminated SQL dollar-quoted string\n" if $content_end < 0;
            my $quoted_length = $content_end + length($delimiter) - $offset;
            $normalized .= substr $sql, $offset, $quoted_length;
            $offset += $quoted_length;
            next;
        }
    }

    $normalized .= $character;
    $offset++;
}

print $normalized;
