CREATE FUNCTION proxy_resource_write_gate() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock_shared(
    hashtextextended(TG_TABLE_SCHEMA || ':proxy-resource-write', 982));
  RETURN NULL;
END;
$$;

CREATE TRIGGER proxy_resource_write_gate
BEFORE INSERT OR UPDATE OR DELETE ON proxies
FOR EACH STATEMENT EXECUTE FUNCTION proxy_resource_write_gate();
