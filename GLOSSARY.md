# OLP

OLP is a gateway that lets teams share model provider accounts and coding subscriptions behind their own keys, routes and budgets.

## Code mode

**Code mode**:
The route-specific forwarding contract that sends a coding client's native requests to a subscription account unchanged.

**Adapter**:
The subscription family a code account belongs to. It decides which client paths the account serves and which clients get generated configuration.
_Avoid_: Vendor, provider (in code-mode contexts)

**Coding plan**:
A vendor subscription for coding tools that an operator enrolls by pasting a non-expiring API key the vendor's key page issues. Vendor brand names such as "Token Plan" or "Kimi Code" are display names of individual coding plans.
_Avoid_: Token plan (as a generic term), API-key subscription

**Profile**:
One account system of an adapter, such as a vendor's global or mainland China service, each with its own key page and upstream host.
_Avoid_: Region, endpoint
