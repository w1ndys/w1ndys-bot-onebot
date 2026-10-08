-- 保留旧词库供回滚，并创建仅包含正权重违禁词的新词库。
ALTER SEQUENCE forbidden_monitor_terms_id_seq
RENAME TO forbidden_monitor_terms_legacy_id_seq;

ALTER TABLE forbidden_monitor_terms RENAME TO forbidden_monitor_terms_legacy;

CREATE TABLE forbidden_monitor_terms (
    id BIGSERIAL PRIMARY KEY,
    text TEXT NOT NULL,
    weight DOUBLE PRECISION NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_forbidden_terms_v2_text CHECK (length(text) BETWEEN 1 AND 100 AND length(btrim(text)) >= 1),
    CONSTRAINT chk_forbidden_terms_v2_weight CHECK (weight > 0 AND weight <= 1000),
    CONSTRAINT chk_forbidden_terms_v2_version CHECK (version > 0),
    CONSTRAINT uq_forbidden_terms_v2_text UNIQUE (text)
);

ALTER TABLE forbidden_monitor_violation_audits
DROP CONSTRAINT chk_forbidden_monitor_violation_score;

ALTER TABLE forbidden_monitor_violation_audits
ALTER COLUMN risk_score TYPE DOUBLE PRECISION
USING risk_score::DOUBLE PRECISION;

ALTER TABLE forbidden_monitor_violation_audits
ADD CONSTRAINT chk_forbidden_monitor_violation_score
CHECK (risk_score IS NULL OR risk_score >= 0);

WITH runtime_threshold AS (
    SELECT CASE
        WHEN jsonb_typeof(config_json -> 'high_threshold') = 'number'
            THEN CASE
                WHEN (config_json ->> 'high_threshold')::NUMERIC = TRUNC((config_json ->> 'high_threshold')::NUMERIC)
                    THEN LEAST(
                        1000::NUMERIC,
                        GREATEST(1::NUMERIC, (config_json ->> 'high_threshold')::NUMERIC)
                    )::DOUBLE PRECISION
                ELSE 60::DOUBLE PRECISION
            END
        ELSE 60::DOUBLE PRECISION
    END AS value
    FROM plugin_runtime_configs
    WHERE plugin_key = 'forbidden_message_monitor'
),
effective_threshold AS (
    SELECT COALESCE((SELECT value FROM runtime_threshold), 60::DOUBLE PRECISION) AS value
),
merged_terms AS (
    SELECT
        legacy.text,
        MAX(CASE WHEN legacy.kind = 'hard' THEN threshold.value ELSE legacy.weight END) AS weight,
        MAX(legacy.version) AS version,
        MIN(legacy.created_at) AS created_at,
        MAX(legacy.updated_at) AS updated_at
    FROM forbidden_monitor_terms_legacy AS legacy
    CROSS JOIN effective_threshold AS threshold
    WHERE legacy.kind IN ('hard', 'risk')
    GROUP BY legacy.text
)
INSERT INTO forbidden_monitor_terms(text, weight, version, created_at, updated_at)
SELECT
    text,
    CASE WHEN weight > 0 THEN weight ELSE 1::DOUBLE PRECISION END,
    version,
    created_at,
    updated_at
FROM merged_terms;

WITH migrated_config AS (
    SELECT
        plugin_key,
        CASE
            WHEN jsonb_typeof(config_json -> 'high_threshold') = 'number'
                THEN CASE
                    WHEN (config_json ->> 'high_threshold')::NUMERIC = TRUNC((config_json ->> 'high_threshold')::NUMERIC)
                        THEN LEAST(
                            1000::NUMERIC,
                            GREATEST(1::NUMERIC, (config_json ->> 'high_threshold')::NUMERIC)
                        )::INTEGER
                    ELSE 60
                END
            ELSE 60
        END AS violation_weight_threshold
    FROM plugin_runtime_configs
    WHERE plugin_key = 'forbidden_message_monitor'
)
UPDATE plugin_runtime_configs AS configs
SET config_json = (
        configs.config_json
        - 'low_threshold'
        - 'high_threshold'
        - 'detection_mode'
    ) || jsonb_build_object('violation_weight_threshold', migrated.violation_weight_threshold),
    version = configs.version + 1,
    updated_at = NOW()
FROM migrated_config AS migrated
WHERE configs.plugin_key = migrated.plugin_key;
