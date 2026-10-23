-- B44.6 — the talyvor.com waitlist: one row per email, written by the suite BFF on the provision secret
-- (POST /v1/waitlist) and read by the operator (GET /v1/admin/waitlist). The email arrives trimmed and
-- lowercased, so UNIQUE (email) is what makes the same address twice one row.
-- Not tenant data: a signup belongs to no workspace, so there is no workspace column and no
-- internal/tenantdata manifest row. No IP and no user agent are kept, on purpose.
CREATE TABLE IF NOT EXISTS waitlist_signups (
    id           BIGSERIAL PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE CHECK (char_length(email) <= 254),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    page         TEXT CHECK (char_length(page) <= 200),
    utm_source   TEXT CHECK (char_length(utm_source) <= 200),
    utm_medium   TEXT CHECK (char_length(utm_medium) <= 200),
    utm_campaign TEXT CHECK (char_length(utm_campaign) <= 200)
);
