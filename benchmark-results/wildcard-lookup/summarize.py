import json, statistics, sys
from pathlib import Path
root=Path(__file__).resolve().parent
source=root/(sys.argv[1] if len(sys.argv)>1 else 'measurements.jsonl')
records=[json.loads(line) for line in source.read_text().splitlines()]
summary=[]
for scenario,mode,concurrency in sorted({(r['scenario'],r['mode'],r['concurrency']) for r in records}):
 case={'scenario':scenario,'mode':mode,'concurrency':concurrency,'variants':{}}
 for variant in sorted({r['variant'] for r in records}):
  rows=[r for r in records if (r['scenario'],r['mode'],r['concurrency'],r['variant'])==(scenario,mode,concurrency,variant)]
  if not rows:continue
  values={
   'p50_ms':[r['p50_ms'] for r in rows],
   'p95_ms':[r['p95_ms'] for r in rows],
   'requests_per_second':[r['requests_per_second'] for r in rows],
   'server_allocated_bytes_per_lookup':[r['metrics_delta']['go_memstats_alloc_bytes_total']/r['successes'] for r in rows],
   'server_cpu_ms_per_lookup':[1000*r['metrics_delta']['process_cpu_seconds_total']/r['successes'] for r in rows],
   'dispatch_cache_hit_percent':[100*r['metrics_delta']['spicedb_dispatch_client_lookup_subjects_from_cache_total']/r['metrics_delta']['spicedb_dispatch_client_lookup_subjects_total'] for r in rows],
  }
  case['variants'][variant]={'repeats':len(rows),'successful_lookups':sum(r['successes'] for r in rows),'lookup_errors':sum(r['errors'] for r in rows),'successful_writes':sum(r['writes'] for r in rows),'write_errors':sum(r['write_errors'] for r in rows),'measurements':{name:{'median':statistics.median(v),'min':min(v),'max':max(v)} for name,v in values.items()}}
 summary.append(case)
report={'source':source.name,'total_lookups':sum(r['successes'] for r in records),'lookup_errors':sum(r['errors'] for r in records),'write_errors':sum(r['write_errors'] for r in records),'summary':summary}
source.with_suffix('.summary.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report,indent=2))
