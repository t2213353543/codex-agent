package server

import "fmt"

// Pre-rollup query retained as an independent exact SQL oracle.
func operationsDetailSQL() string {
	return fmt.Sprintf(`WITH source AS MATERIALIZED (
 SELECT floor((extract(epoch FROM created_at)-$1::bigint)/$3::bigint)::bigint AS point,
 status,(%s) AS category,
 COALESCE((record->>'input_tokens')::bigint,0)+COALESCE((record->>'output_tokens')::bigint,0) AS tokens,
 COALESCE((record->>'usage_known')::boolean,COALESCE((record->>'input_tokens')::bigint,0)<>0 OR COALESCE((record->>'output_tokens')::bigint,0)<>0 OR COALESCE((record->>'cached_tokens')::bigint,0)<>0) AS usage_known,
 CASE WHEN COALESCE((record->>'latency_known')::boolean,COALESCE((record->>'latency_ms')::bigint,0)>0) AND COALESCE((record->>'latency_ms')::bigint,0)>=0 THEN COALESCE((record->>'latency_ms')::bigint,0) END AS latency,
 CASE WHEN (record->>'ttft_ms')::bigint>=0 THEN (record->>'ttft_ms')::bigint END AS ttft
 FROM request_logs WHERE created_at>=to_timestamp($1) AND created_at<to_timestamp($2)
 ), aggregates AS (
 SELECT CASE WHEN grouping(point)=0 THEN 'series' WHEN grouping(category)=0 THEN 'category' ELSE 'total' END AS kind,
 COALESCE(point,0) AS point,COALESCE(category,'') AS category,
 count(*)::bigint AS requests,count(*) FILTER(WHERE status='ok')::bigint AS success,
 count(*) FILTER(WHERE status<>'ok')::bigint AS errors,count(*) FILTER(WHERE status='aborted')::bigint AS aborted,
 COALESCE(sum(tokens),0)::bigint AS tokens,count(*) FILTER(WHERE usage_known)::bigint AS usage_known,
 count(latency)::bigint AS latency_samples,avg(latency)::float8 AS latency,
 count(ttft)::bigint AS ttft_samples,avg(ttft)::float8 AS ttft,
 ARRAY[count(*) FILTER(WHERE latency<=1000),count(*) FILTER(WHERE latency>1000 AND latency<=5000),
 count(*) FILTER(WHERE latency>5000 AND latency<=15000),count(*) FILTER(WHERE latency>15000 AND latency<=30000),
 count(*) FILTER(WHERE latency>30000 AND latency<=60000),count(*) FILTER(WHERE latency>60000 AND latency<=180000),
 count(*) FILTER(WHERE latency>180000)]::bigint[] AS histogram
 FROM source GROUP BY GROUPING SETS ((point),(category),())
 ) SELECT kind,point,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency,ttft_samples,ttft,histogram,NULL::jsonb AS recent
 FROM aggregates WHERE kind<>'category' OR category<>''
 UNION ALL
 SELECT 'recent',0,'',0,0,0,0,0,0,0,NULL::float8,0,NULL::float8,ARRAY[]::bigint[],
 (SELECT COALESCE(jsonb_agg(item ORDER BY stamp DESC,id DESC),'[]'::jsonb) FROM (
 SELECT id,created_at AS stamp,jsonb_build_object('id',id,'time',floor(extract(epoch FROM created_at))::bigint,
 'model',model,'status',status,'http_code',COALESCE((record->>'http_code')::integer,0),'category',(%s)) AS item
 FROM request_logs WHERE created_at>=to_timestamp($1) AND created_at<to_timestamp($2) AND status<>'ok'
 ORDER BY created_at DESC,id DESC LIMIT 8) recent)`, managedFailureSQL, managedFailureSQL)
}
