-- A global hour summarizes the canonical minute table, including its legacy
-- global baselines. usage_daily has independent account baselines and cannot
-- replace global minute totals without changing historical results.
CREATE TABLE usage_hourly_global (
    bucket bigint PRIMARY KEY,
    requests bigint NOT NULL,
    ok bigint NOT NULL,
    errors bigint NOT NULL,
    tokens bigint NOT NULL,
    input bigint NOT NULL,
    output bigint NOT NULL,
    cached bigint NOT NULL,
    latency bigint NOT NULL,
    latency_n bigint NOT NULL
);

-- Lock the backfill boundary against writers; ordinary readers keep working.
LOCK TABLE usage_minute IN SHARE ROW EXCLUSIVE MODE;
INSERT INTO usage_hourly_global
SELECT (bucket/3600)*3600,sum(requests),sum(ok),sum(errors),sum(tokens),sum(input),sum(output),sum(cached),sum(latency),sum(latency_n)
FROM usage_minute GROUP BY 1;

CREATE FUNCTION maintain_usage_hourly_global() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Each change contributes its delta, not the complete minute a second time.
    -- ORDER BY gives moves between hours a deterministic row-lock order.
    INSERT INTO usage_hourly_global AS hourly
    SELECT (bucket/3600)*3600,sum(requests),sum(ok),sum(errors),sum(tokens),sum(input),sum(output),sum(cached),sum(latency),sum(latency_n)
    FROM (
        SELECT NEW.bucket,NEW.requests,NEW.ok,NEW.errors,NEW.tokens,NEW.input,NEW.output,NEW.cached,NEW.latency,NEW.latency_n
        WHERE TG_OP<>'DELETE'
        UNION ALL
        SELECT OLD.bucket,0,0,0,0,0,0,0,0,0
        WHERE TG_OP<>'INSERT'
    ) changes
    GROUP BY 1 ORDER BY 1
    ON CONFLICT(bucket) DO UPDATE SET
        requests=hourly.requests::numeric+EXCLUDED.requests-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.requests::numeric ELSE 0 END,
        ok=hourly.ok::numeric+EXCLUDED.ok-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.ok::numeric ELSE 0 END,
        errors=hourly.errors::numeric+EXCLUDED.errors-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.errors::numeric ELSE 0 END,
        tokens=hourly.tokens::numeric+EXCLUDED.tokens-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.tokens::numeric ELSE 0 END,
        input=hourly.input::numeric+EXCLUDED.input-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.input::numeric ELSE 0 END,
        output=hourly.output::numeric+EXCLUDED.output-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.output::numeric ELSE 0 END,
        cached=hourly.cached::numeric+EXCLUDED.cached-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.cached::numeric ELSE 0 END,
        latency=hourly.latency::numeric+EXCLUDED.latency-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.latency::numeric ELSE 0 END,
        latency_n=hourly.latency_n::numeric+EXCLUDED.latency_n-CASE WHEN TG_OP<>'INSERT' AND (OLD.bucket/3600)*3600=EXCLUDED.bucket THEN OLD.latency_n::numeric ELSE 0 END;
    RETURN NULL;
END
$$;
CREATE TRIGGER usage_minute_hourly_change AFTER INSERT OR UPDATE OR DELETE ON usage_minute
    FOR EACH ROW EXECUTE FUNCTION maintain_usage_hourly_global();

CREATE FUNCTION truncate_usage_hourly_global() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE usage_hourly_global;
    RETURN NULL;
END
$$;
CREATE TRIGGER usage_minute_hourly_truncate AFTER TRUNCATE ON usage_minute
    FOR EACH STATEMENT EXECUTE FUNCTION truncate_usage_hourly_global();
