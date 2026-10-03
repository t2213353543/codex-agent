package server

import "fmt"

const retainedOperationColumns = "requests,success,errors,aborted,tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,latency_0,latency_1,latency_2,latency_3,latency_4,latency_5,latency_6"

// Each slot [stamp,stamp+step) contributes its whole-minute span to the
// minute summaries and the remaining seconds to the second summaries. The
// per-slot pieces are closed forms, so both summaries are single range scans
// instead of one lateral index probe per piece:
//   - minute bucket b is fully inside some slot iff step>=60 and
//     b>=from and b+60<=to and (b-from)%%step<=step-60; its point is the slot
//     index (b-from)/step.
//   - a second-row bucket belongs to exactly one slot (slots tile the window),
//     so its point is (bucket-from)/step and it is excluded exactly when its
//     minute satisfies the whole-minute condition above.
// Bucket range filters keep both branches on the (resolution,bucket) prefix
// of the primary key; the modulo conditions are plain rechecks.
func operationsRollupSQL() string {
	return fmt.Sprintf(`WITH source AS MATERIALIZED (
 SELECT (s.bucket-$1::bigint)/$3::bigint AS point,s.category,%s FROM retained_operations s
 WHERE s.resolution=60 AND s.requests>0 AND $3::bigint>=60 AND s.bucket>=$1::bigint AND s.bucket+60<=$2::bigint
 AND (s.bucket-$1::bigint)%%$3::bigint<=$3::bigint-60
 UNION ALL
 SELECT (s.bucket-$1::bigint)/$3::bigint AS point,s.category,%s FROM retained_operations s
 WHERE s.resolution=1 AND s.requests>0 AND s.bucket>=$1::bigint AND s.bucket<$2::bigint
 AND NOT ($3::bigint>=60 AND s.bucket/60*60>=$1::bigint AND s.bucket/60*60+60<=$2::bigint AND (s.bucket/60*60-$1::bigint)%%$3::bigint<=$3::bigint-60)
 ), aggregates AS (
 SELECT CASE WHEN grouping(point)=0 THEN 'series' WHEN grouping(category)=0 THEN 'category' ELSE 'total' END AS kind,
 COALESCE(point,0) AS point,COALESCE(category,'') AS category,
 COALESCE(sum(requests),0)::bigint AS requests,COALESCE(sum(success),0)::bigint AS success,
 COALESCE(sum(errors),0)::bigint AS errors,COALESCE(sum(aborted),0)::bigint AS aborted,
 COALESCE(sum(tokens),0)::bigint AS tokens,COALESCE(sum(usage_known),0)::bigint AS usage_known,
 COALESCE(sum(latency_samples),0)::bigint AS latency_samples,(sum(latency_sum)/NULLIF(sum(latency_samples),0))::float8 AS latency,
 COALESCE(sum(ttft_samples),0)::bigint AS ttft_samples,(sum(ttft_sum)/NULLIF(sum(ttft_samples),0))::float8 AS ttft,
 ARRAY[COALESCE(sum(latency_0),0),COALESCE(sum(latency_1),0),COALESCE(sum(latency_2),0),COALESCE(sum(latency_3),0),
 COALESCE(sum(latency_4),0),COALESCE(sum(latency_5),0),COALESCE(sum(latency_6),0)]::bigint[] AS histogram
 FROM source GROUP BY GROUPING SETS ((point),(category),())
 ) SELECT kind,point,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency,ttft_samples,ttft,histogram,NULL::jsonb AS recent
 FROM aggregates WHERE kind<>'category' OR category<>''
 UNION ALL
 SELECT 'recent',0,'',0,0,0,0,0,0,0,NULL::float8,0,NULL::float8,ARRAY[]::bigint[],
 (SELECT COALESCE(jsonb_agg(item ORDER BY stamp DESC,id DESC),'[]'::jsonb) FROM (
 SELECT id,created_at AS stamp,jsonb_build_object('id',id,'time',floor(extract(epoch FROM created_at))::bigint,
 'model',model,'status',status,'http_code',COALESCE((record->>'http_code')::integer,0),'category',(%s)) AS item
 FROM request_logs WHERE created_at>=to_timestamp($1::bigint) AND created_at<to_timestamp($2::bigint) AND status<>'ok'
 ORDER BY created_at DESC,id DESC LIMIT 8) recent)`, retainedOperationColumns, retainedOperationColumns, managedFailureSQL)
}
