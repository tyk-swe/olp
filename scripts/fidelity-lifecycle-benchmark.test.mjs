import assert from 'node:assert/strict';
import { test } from 'node:test';
import { names, validateRuns, parseRuns, freeze, compare } from './fidelity-lifecycle-benchmark.mjs';
function evidence() {
 const runs = names.flatMap((name) => [0,1,2].map((repetition) => {
  const metrics = Object.fromEntries(['elapsed_ns','ns/op','process-cpu-ns/op','B/op','allocs/op','sampled-heap-growth-B'].map((metric) => [metric,100]));
  for (const c of ['latency',...(name.startsWith('durable') ? ['publication'] : ['cancellation','event-roundtrip'])]) for (const p of [50,95,99]) metrics[`${c}-p${p}-us`] = 100;
  return { name,repetition,samples:24,succeeded:24,dispatches:24,events_per_request:name.startsWith('durable_unary') ? 0 : name.startsWith('durable_stream') ? 66 : 64,metrics };
 }));
 return {schema:'openllmproxy.dev/fidelity-lifecycle-performance/v1',contract:null,working_tree:'',harness_sha256:'fixture',runner_sha256:'fixture',hardware:{cpu:'fixture'},storage:{postgresql_server_version:'180004'},runs};
}
test('complete fixed lifecycle inventory passes and log parsing retains every metric',()=>{
 const a=evidence();assert.equal(Object.keys(validateRuns(a.runs)).length,16);assert.deepEqual(compare(a,freeze(a)),[]);
 assert.deepEqual(parseRuns(a.runs.map(r=>` x: LIFECYCLE_MEASUREMENT ${JSON.stringify(r)}`).join('\n')),a.runs);
});
test('missing work, samples, dispatches, terminal events or metrics cannot pass',()=>{
 for (const mutate of [a=>a.runs.pop(),a=>a.runs[0].samples--,a=>a.runs[0].dispatches--,a=>a.runs[0].succeeded--,a=>a.runs[1].events_per_request++,a=>delete a.runs[0].metrics['publication-p99-us'],a=>a.runs[0].metrics['B/op']=NaN,a=>a.runs[1]=structuredClone(a.runs[0])]) {
  const a=evidence();const b=freeze(a);mutate(a);assert.throws(()=>compare(a,b));
 }
});
test('slower state publication and cancellation independently fail frozen budgets',()=>{
 const a=evidence();const b=freeze(a);
 for(const r of a.runs)if(r.name.endsWith('/gateway')) r.metrics[r.name.startsWith('durable')?'publication-p99-us':'cancellation-p99-us']=5000;
 assert.equal(compare(a,b).length,8);
});
test('changed evidence identity or conditions and unmeasured strict mode are refused',()=>{
 for(const mutate of [a=>a.harness_sha256='changed',a=>a.runner_sha256='changed',a=>a.hardware.cpu='different',a=>a.storage.postgresql_server_version='190001',a=>a.contract={mode:'strict'}]) {
  const a=evidence();const b=freeze(a);mutate(a);assert.throws(()=>compare(a,b));
 }
 const a=evidence();const b=freeze(a);assert.throws(()=>compare(a,b,'strict'));a.contract={mode:'strict'};assert.deepEqual(compare(a,b,'strict'),[]);assert.throws(()=>freeze(a));
});
test('edited budget inventory and invalid bounds cannot weaken required comparisons',()=>{
 for(const mutate of [b=>delete b.maxima[names[0]],b=>delete b.maxima[names[0]]['B/op'],b=>b.maxima[names[0]]['B/op']=NaN]){
  const a=evidence();const b=freeze(a);mutate(b);assert.throws(()=>compare(a,b));
 }
});
