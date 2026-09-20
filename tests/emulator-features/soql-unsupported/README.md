# Unsupported SOQL is named, not called malformed

A clause the emulator declines is valid SOQL. Reporting `MALFORMED_QUERY` for it sends the
caller to re-read a query whose syntax is fine, so these answer `UNSUPPORTED_SOQL` and name
the clause instead.

`UNSUPPORTED_SOQL` is not a Salesforce error code. That is deliberate: a code no real org
emits is the honest signal that the limit is ours rather than the platform's.

The third test guards the other direction. A real syntax error must keep the code a real org
returns, or the new code becomes a catch-all that tells the caller nothing.
