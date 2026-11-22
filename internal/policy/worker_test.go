package policy

import "testing"

func TestRiskIn(t *testing.T) {
	risky := []string{
		"Rotate the API token for the build server.",
		"Fix the OAuth callback URL.",
		"Write the database migration for the orders table.",
		"Handle the payment webhook retries.",
		"Fix the login redirect after sign-up.",
		"Add a Stripe webhook handler.",
		"Run the database migration.",
		"Review the payment flow.",
		"Change the nginx config for the new host.",
		"Open port 8443 on the staging box.",
		"Store the password hash with bcrypt.",
		"Deploy to production after the merge.",
		"Add two-factor authentication to the admin.",
		"Fix the client authenticating against the gateway.",
		"Only authenticated users may see the report.",
		"Check the authorization middleware.",
		"The session cookie must be HttpOnly.",
		"Read the secret from the vault, then rotate the access token.",
		"Send the bearer header on every call.",
		"Update the checkout flow copy and totals.",
		"Migrate the database to the new host.",
		"Do not deploy yet; first rotate the API token.",
		"internal/auth/session.go",
		"db/migrations/0003_orders.sql",
		"Add the refund button to the invoice page.",
	}
	benign := []string{
		"Update the author line in the changelog.",
		"Keep the token budget under 20000.",
		"Do not deploy.",
		"Never commit secrets or keys to the repository.",
		"Speed up the tokenizer.",
		"The authored commit message is too long.",
		"Count the tokens in the prompt.",
		"Set max_tokens to 4096.",
		"Do not deploy anything; open a PR instead.",
		"Update the AUTHORS file.",
		"Fix the typo in the README.",
		"Ask the authority on the style guide.",
		"Run git checkout main before you start.",
		"Migrate the tests to the new runner.",
		"Tokenize the input before counting.",
		"You must not deploy from a worker.",
		"No need to deploy; the coordinator releases.",
		"guides/author-guide.md",
		"internal/outcomes/collect.go",
		"Rename the helper and add a test.",
	}
	if len(risky) < 15 || len(benign) < 15 {
		t.Fatalf("table too small: %d risky, %d benign", len(risky), len(benign))
	}
	for _, s := range risky {
		if !RiskIn(s) {
			t.Errorf("RiskIn(%q) = false, want true", s)
		}
	}
	for _, s := range benign {
		if RiskIn(s) {
			t.Errorf("RiskIn(%q) = true, want false", s)
		}
	}
}
