"""Compare retained preparation reports with an independently pinned Spec reader.
Usage: python tools/verify-preparation-parity.py SPEC/tools/dialect_verify.py RUNNER
"""
import pathlib,json,subprocess,tempfile,hashlib,importlib.util,sys
root=pathlib.Path(__file__).resolve().parents[1];spec=pathlib.Path(sys.argv[1]).resolve();runner=pathlib.Path(sys.argv[2]).resolve()
m=importlib.util.spec_from_file_location('fixture_spec_reader',spec);reader=importlib.util.module_from_spec(m);sys.modules[m.name]=reader;m.loader.exec_module(reader)
fail=[]
for vector in json.loads((root/'vectors/preparation/manifest.json').read_text())['vectors']:
 d=root/'vectors/preparation'/vector['name']
 with tempfile.TemporaryDirectory(prefix='jps-verify-parity-') as td:
  temp=pathlib.Path(td)
  prep=d/'preparation.json'
  if vector['preparation']['retained']!='equal':
   run=reader.read_document((d/'run.json').read_bytes(),'run');prep=temp/'preparation.json';prep.write_bytes(reader.preparation_bytes(run['run']['input']['preparation']))
  common=['--dialect',str(d/('unbound.json' if (d/'binding.json').exists() else 'mapping.json')),'--case',str(d/'case.json'),'--profiles',str(d/'profiles.json'),'--preparation',str(prep)]
  if (d/'sources.json').exists():common+=['--acquisitions',str(d/'sources.json')]
  if (d/'binding.json').exists():common+=['--binding',str(d/'binding.json')]
  a=subprocess.run([str(runner),'verify-preparation',*common,'--expected-digest','sha256:'+hashlib.sha256(prep.read_bytes()).hexdigest()],capture_output=True,text=True,timeout=30)
  b=subprocess.run([sys.executable,str(spec),*common],capture_output=True,text=True,timeout=30)
  try:equal=json.loads(a.stdout)==json.loads(b.stdout) and a.returncode==b.returncode
  except Exception:equal=False
  if not equal:fail.append({'name':vector['name'],'runner':a.stdout,'spec':b.stdout,'errors':a.stderr+b.stderr,'codes':[a.returncode,b.returncode]})
print('41 vectors compared; differences:',len(fail));print('\n'.join(f['name'] for f in fail));sys.exit(bool(fail))
