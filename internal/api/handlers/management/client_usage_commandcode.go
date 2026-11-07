package management

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// clientUsageCommandCodeProvider is the provider key of Command Code
// (commandcode.ai) logins, which are OAuth-kind credentials holding an API key.
const clientUsageCommandCodeProvider = "commandcode"

// Command Code quota endpoints, the same ones the panel's quota card reads
// with the credential's API key as Bearer token.
var (
	clientUsageCommandCodeCreditsURL      = "https://api.commandcode.ai/alpha/billing/credits"
	clientUsageCommandCodeSubscriptionURL = "https://api.commandcode.ai/alpha/billing/subscriptions"
)

// probeCommandCodeClientUsage returns the credit windows and the subscription
// plan the panel combines into its Command Code quota card.
func (h *Handler) probeCommandCodeClientUsage(ctx context.Context, auth *coreauth.Auth, token string) []clientUsageUpstream {
	headers := map[string]string{"Accept": "application/json"}
	return h.clientUsageRequests(ctx, auth, token, []clientUsageCall{
		{kind: clientUsageKindUsage, url: clientUsageCommandCodeCreditsURL, headers: headers},
		{kind: clientUsageKindSubscription, optional: true, url: clientUsageCommandCodeSubscriptionURL, headers: headers},
	})
}
