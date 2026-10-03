-- Account ranking follows canonical usage_minute, including the minute's
-- current label. usage_daily has independent legacy baselines and must not be
-- substituted. Day is a UTC day ordinal, keeping all signed bigint buckets safe.
CREATE TABLE usage_account_top_day (
    day bigint NOT NULL,
    account_ref text NOT NULL,
    account_name text NOT NULL,
    minute_rows numeric NOT NULL,
    requests numeric NOT NULL,
    errors numeric NOT NULL,
    tokens numeric NOT NULL,
    PRIMARY KEY(day,account_ref,account_name)
);
CREATE TABLE usage_account_top_unsafe_day (
    day bigint PRIMARY KEY,
    minute_rows numeric NOT NULL
);

-- Keep database-default collation for grouping, MAX(label), and stable ties.
-- Legal long names are represented by a day guard rather than rejected by a
-- btree tuple limit. Guarded days are read from the canonical minute relation.
LOCK TABLE usage_minute IN SHARE ROW EXCLUSIVE MODE;
WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,1::numeric AS minute_rows,
        1*requests::numeric AS requests,1*errors::numeric AS errors,1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM usage_minute WHERE account_name<>'')
    INSERT INTO usage_account_top_unsafe_day AS guard(day,minute_rows)
    SELECT day,sum(minute_rows) FROM changes WHERE NOT safe GROUP BY day ORDER BY day
    ON CONFLICT(day) DO UPDATE SET minute_rows=guard.minute_rows+EXCLUDED.minute_rows;
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,1::numeric AS minute_rows,
        1*requests::numeric AS requests,1*errors::numeric AS errors,1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM usage_minute WHERE account_name<>'')
    INSERT INTO usage_account_top_day AS summary(day,account_ref,account_name,minute_rows,requests,errors,tokens)
    SELECT day,account_ref,account_name,sum(minute_rows),sum(requests),sum(errors),sum(tokens)
    FROM changes WHERE safe GROUP BY day,account_ref,account_name ORDER BY day,account_ref,account_name
    ON CONFLICT(day,account_ref,account_name) DO UPDATE SET
        minute_rows=summary.minute_rows+EXCLUDED.minute_rows,
        requests=summary.requests+EXCLUDED.requests,
        errors=summary.errors+EXCLUDED.errors,
        tokens=summary.tokens+EXCLUDED.tokens;

-- Group transition rows before touching derived rows. Numeric signed deltas
-- preserve cancellation and avoid overflowing when decrementing INT64_MIN.

CREATE FUNCTION maintain_usage_account_top_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,1::numeric AS minute_rows,
        1*requests::numeric AS requests,1*errors::numeric AS errors,1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_new WHERE account_name<>'')
    INSERT INTO usage_account_top_unsafe_day AS guard(day,minute_rows)
    SELECT day,sum(minute_rows) FROM changes WHERE NOT safe GROUP BY day ORDER BY day
    ON CONFLICT(day) DO UPDATE SET minute_rows=guard.minute_rows+EXCLUDED.minute_rows;
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,1::numeric AS minute_rows,
        1*requests::numeric AS requests,1*errors::numeric AS errors,1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_new WHERE account_name<>'')
    INSERT INTO usage_account_top_day AS summary(day,account_ref,account_name,minute_rows,requests,errors,tokens)
    SELECT day,account_ref,account_name,sum(minute_rows),sum(requests),sum(errors),sum(tokens)
    FROM changes WHERE safe GROUP BY day,account_ref,account_name ORDER BY day,account_ref,account_name
    ON CONFLICT(day,account_ref,account_name) DO UPDATE SET
        minute_rows=summary.minute_rows+EXCLUDED.minute_rows,
        requests=summary.requests+EXCLUDED.requests,
        errors=summary.errors+EXCLUDED.errors,
        tokens=summary.tokens+EXCLUDED.tokens;
    RETURN NULL;
END
$$;
CREATE TRIGGER usage_minute_account_top_insert AFTER INSERT ON usage_minute
    REFERENCING  NEW TABLE AS account_top_new
    FOR EACH STATEMENT EXECUTE FUNCTION maintain_usage_account_top_insert();

CREATE FUNCTION maintain_usage_account_top_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,1::numeric AS minute_rows,
        1*requests::numeric AS requests,1*errors::numeric AS errors,1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_new WHERE account_name<>''
    UNION ALL
    SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,-1::numeric AS minute_rows,
        -1*requests::numeric AS requests,-1*errors::numeric AS errors,-1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_old WHERE account_name<>'')
    INSERT INTO usage_account_top_unsafe_day AS guard(day,minute_rows)
    SELECT day,sum(minute_rows) FROM changes WHERE NOT safe GROUP BY day ORDER BY day
    ON CONFLICT(day) DO UPDATE SET minute_rows=guard.minute_rows+EXCLUDED.minute_rows;
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,1::numeric AS minute_rows,
        1*requests::numeric AS requests,1*errors::numeric AS errors,1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_new WHERE account_name<>''
    UNION ALL
    SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,-1::numeric AS minute_rows,
        -1*requests::numeric AS requests,-1*errors::numeric AS errors,-1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_old WHERE account_name<>'')
    INSERT INTO usage_account_top_day AS summary(day,account_ref,account_name,minute_rows,requests,errors,tokens)
    SELECT day,account_ref,account_name,sum(minute_rows),sum(requests),sum(errors),sum(tokens)
    FROM changes WHERE safe GROUP BY day,account_ref,account_name ORDER BY day,account_ref,account_name
    ON CONFLICT(day,account_ref,account_name) DO UPDATE SET
        minute_rows=summary.minute_rows+EXCLUDED.minute_rows,
        requests=summary.requests+EXCLUDED.requests,
        errors=summary.errors+EXCLUDED.errors,
        tokens=summary.tokens+EXCLUDED.tokens;
    RETURN NULL;
END
$$;
CREATE TRIGGER usage_minute_account_top_update AFTER UPDATE ON usage_minute
    REFERENCING OLD TABLE AS account_top_old NEW TABLE AS account_top_new
    FOR EACH STATEMENT EXECUTE FUNCTION maintain_usage_account_top_update();

CREATE FUNCTION maintain_usage_account_top_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,-1::numeric AS minute_rows,
        -1*requests::numeric AS requests,-1*errors::numeric AS errors,-1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_old WHERE account_name<>'')
    INSERT INTO usage_account_top_unsafe_day AS guard(day,minute_rows)
    SELECT day,sum(minute_rows) FROM changes WHERE NOT safe GROUP BY day ORDER BY day
    ON CONFLICT(day) DO UPDATE SET minute_rows=guard.minute_rows+EXCLUDED.minute_rows;
    WITH changes AS MATERIALIZED (SELECT floor(bucket::numeric/86400)::bigint AS day,account_ref,account_name,-1::numeric AS minute_rows,
        -1*requests::numeric AS requests,-1*errors::numeric AS errors,-1*tokens::numeric AS tokens,
        octet_length(account_ref)+octet_length(account_name)<=1700 AS safe
    FROM account_top_old WHERE account_name<>'')
    INSERT INTO usage_account_top_day AS summary(day,account_ref,account_name,minute_rows,requests,errors,tokens)
    SELECT day,account_ref,account_name,sum(minute_rows),sum(requests),sum(errors),sum(tokens)
    FROM changes WHERE safe GROUP BY day,account_ref,account_name ORDER BY day,account_ref,account_name
    ON CONFLICT(day,account_ref,account_name) DO UPDATE SET
        minute_rows=summary.minute_rows+EXCLUDED.minute_rows,
        requests=summary.requests+EXCLUDED.requests,
        errors=summary.errors+EXCLUDED.errors,
        tokens=summary.tokens+EXCLUDED.tokens;
    RETURN NULL;
END
$$;
CREATE TRIGGER usage_minute_account_top_delete AFTER DELETE ON usage_minute
    REFERENCING OLD TABLE AS account_top_old 
    FOR EACH STATEMENT EXECUTE FUNCTION maintain_usage_account_top_delete();

CREATE FUNCTION truncate_usage_account_top() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE usage_account_top_day,usage_account_top_unsafe_day;
    RETURN NULL;
END
$$;
CREATE TRIGGER usage_minute_account_top_truncate AFTER TRUNCATE ON usage_minute
    FOR EACH STATEMENT EXECUTE FUNCTION truncate_usage_account_top();

