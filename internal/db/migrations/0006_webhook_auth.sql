-- An optional Authorization header value per rewrite webhook, for
-- receivers that require one (for example a webhook routine that checks
-- "Authorization: Bearer ..."). AES-GCM sealed with the master key like the
-- sender key, with its own AAD; NULL means not set. Never shown again.
ALTER TABLE agent_webhooks ADD COLUMN auth_sealed BLOB;
