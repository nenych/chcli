-- The role every token-authenticated user receives (see token-auth.xml).
CREATE ROLE IF NOT EXISTS chcli_token_reader;
GRANT SELECT ON system.* TO chcli_token_reader;
