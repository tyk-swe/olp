macro_rules! closed_string_enum {
    (
        $visibility:vis enum $name:ident {
            $($variant:ident => $wire:literal),+ $(,)?
        }
        parse_error $error:ty => $invalid:expr;
    ) => {
        #[derive(
            Clone,
            Copy,
            Debug,
            serde::Deserialize,
            Eq,
            Hash,
            Ord,
            PartialEq,
            PartialOrd,
            serde::Serialize,
            utoipa::ToSchema,
        )]
        $visibility enum $name {
            $(
                #[serde(rename = $wire)]
                #[schema(rename = $wire)]
                $variant,
            )+
        }

        impl $name {
            $visibility const ALL: [Self; closed_string_enum!(@count $($variant),+)] =
                [$(Self::$variant),+];

            #[must_use]
            $visibility const fn as_str(self) -> &'static str {
                match self {
                    $(Self::$variant => $wire),+
                }
            }
        }

        impl AsRef<str> for $name {
            fn as_ref(&self) -> &str {
                self.as_str()
            }
        }

        impl std::fmt::Display for $name {
            fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str(self.as_str())
            }
        }

        impl std::str::FromStr for $name {
            type Err = $error;

            fn from_str(value: &str) -> Result<Self, Self::Err> {
                match value {
                    $($wire => Ok(Self::$variant),)+
                    _ => Err(($invalid)(value)),
                }
            }
        }
    };
    (@count $head:ident $(, $tail:ident)*) => {
        1usize + closed_string_enum!(@count $($tail),*)
    };
    (@count) => { 0usize };
}
