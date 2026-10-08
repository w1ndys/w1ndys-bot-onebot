-- 将新词库中的当前值合并为旧词库的风险词，并保留旧硬词与安全词。
ALTER TABLE forbidden_monitor_violation_audits
DROP CONSTRAINT chk_forbidden_monitor_violation_score;

ALTER TABLE forbidden_monitor_violation_audits
ALTER COLUMN risk_score TYPE SMALLINT
USING CASE
    WHEN risk_score IS NULL THEN NULL
    ELSE LEAST(100::DOUBLE PRECISION, GREATEST(0::DOUBLE PRECISION, ROUND(risk_score)))::SMALLINT
END;

ALTER TABLE forbidden_monitor_violation_audits
ADD CONSTRAINT chk_forbidden_monitor_violation_score
CHECK (risk_score IS NULL OR risk_score BETWEEN 0 AND 100);

INSERT INTO forbidden_monitor_terms_legacy(kind, text, weight, version, created_at, updated_at)
SELECT 'risk', text, weight, version, created_at, updated_at
FROM forbidden_monitor_terms
ON CONFLICT (kind, text) DO UPDATE
SET weight = EXCLUDED.weight,
    version = GREATEST(forbidden_monitor_terms_legacy.version, EXCLUDED.version) + 1,
    updated_at = NOW();

DROP TABLE forbidden_monitor_terms;
ALTER TABLE forbidden_monitor_terms_legacy RENAME TO forbidden_monitor_terms;
ALTER SEQUENCE forbidden_monitor_terms_legacy_id_seq
RENAME TO forbidden_monitor_terms_id_seq;

WITH restored_config AS (
    SELECT
        plugin_key,
        CASE
            WHEN jsonb_typeof(config_json -> 'violation_weight_threshold') = 'number'
                THEN CASE
                    WHEN (config_json ->> 'violation_weight_threshold')::NUMERIC = TRUNC((config_json ->> 'violation_weight_threshold')::NUMERIC)
                        THEN LEAST(
                            1000::NUMERIC,
                            GREATEST(1::NUMERIC, (config_json ->> 'violation_weight_threshold')::NUMERIC)
                        )::INTEGER
                    ELSE 60
                END
            ELSE 60
        END AS high_threshold
    FROM plugin_runtime_configs
    WHERE plugin_key = 'forbidden_message_monitor'
)
UPDATE plugin_runtime_configs AS configs
SET config_json = (
        configs.config_json
        - 'violation_weight_threshold'
    ) || jsonb_build_object(
        'low_threshold', 20,
        'high_threshold', restored.high_threshold,
        'detection_mode', 'standard'
    ),
    version = configs.version + 1,
    updated_at = NOW()
FROM restored_config AS restored
WHERE configs.plugin_key = restored.plugin_key;
