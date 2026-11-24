-- B61.100 — the BFF asks which person holds an identity at a Google sign-in (POST /v1/auth/person/by-identity); each
-- ask is a row in the door's one audit trail, phone_signin_events, with the person's number when one holds it.
ALTER TABLE phone_signin_events DROP CONSTRAINT IF EXISTS phone_signin_events_event_check;
ALTER TABLE phone_signin_events ADD CONSTRAINT phone_signin_events_event_check CHECK (event IN ('started', 'finished',
    'wrong_code', 'ended', 'refused', 'limited', 'joined', 'signed_in', 'key_signed_in', 'key_refused', 'key_added',
    'key_removed', 'number_moved', 'identity_looked_up'));
