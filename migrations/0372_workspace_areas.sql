-- 0372: B59.1 — the areas each workspace has. Everything outside the launch pack is an area, and every area is
-- off for every workspace, new and existing: a workspace has an area only while it has a row here, and only the
-- operator writes one (PUT /v1/admin/workspaces/{wsID}/areas/{area}), each switch on or off recorded in
-- operator_audit in the same transaction. A route of an area the workspace does not have answers 404.
CREATE TABLE IF NOT EXISTS workspace_areas (
    workspace_id   TEXT        NOT NULL,
    area           TEXT        NOT NULL CHECK (area IN ('marketplace', 'rooms', 'chat', 'track', 'docs', 'code', 'edge',
                                                        'desktop', 'scan', 'counter', 'trading', 'credit', 'interest',
                                                        'prediction_markets', 'gateway')),
    switched_on_by TEXT        NOT NULL,
    switched_on_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, area)
);
