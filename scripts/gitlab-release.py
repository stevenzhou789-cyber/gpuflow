#!/usr/bin/env python3
"""Community's numbered GitLab channel; development builds cannot publish it."""
import argparse, base64, hashlib, json, os, pathlib, re, subprocess, sys
import urllib.error, urllib.parse, urllib.request
from datetime import datetime

ROOT = pathlib.Path(__file__).resolve().parents[1]
GUARD = ROOT / "gitlab-evidence/formal-tag-guard.json"
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
SHA = re.compile(r"[a-f0-9]{40}\Z")

def require(ok, message):
    if not ok: raise ValueError(message)

def canonical(value):
    return (json.dumps(value,sort_keys=True,separators=(",",":"))+"\n").encode()

def run(*args, env=None):
    result = subprocess.run(args,cwd=ROOT,env=env,capture_output=True,text=True)
    require(result.returncode==0, args[0]+" failed; release stopped")
    return result.stdout.strip()

def hash_file(path):
    with path.open("rb") as source: return hashlib.file_digest(source,"sha256").hexdigest()

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args,**kwargs): return None

class API:
    def __init__(self,env):
        self.env=env
        server=env.get("CI_SERVER_URL","").rstrip("/")
        require(server in ("http://gitlab.gpuflow.test:8088","http://127.0.0.1:8088","http://localhost:8088"),"Unexpected GitLab endpoint")
        require(re.fullmatch(r"[1-9][0-9]*",env.get("CI_PROJECT_ID","")),"Invalid project ID")
        token=env.get("GITLAB_RELEASE_TOKEN") or env.get("CI_JOB_TOKEN")
        require(token,"Protected GitLab credential is required")
        self.base=server+"/api/v4/projects/"+env["CI_PROJECT_ID"]
        self.headers={"PRIVATE-TOKEN" if env.get("GITLAB_RELEASE_TOKEN") else "JOB-TOKEN":token}
        self.opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
    def request(self,method,path,data=None,missing=False,length=None):
        headers=dict(self.headers)
        if data is not None: headers["Content-Type"]="application/octet-stream" if length is not None else "application/json"
        if length is not None: headers["Content-Length"]=str(length)
        try: return self.opener.open(urllib.request.Request(self.base+path,data=data,headers=headers,method=method),timeout=120)
        except urllib.error.HTTPError as error:
            if missing and error.code==404: error.close();return None
            raise ValueError("GitLab API refused release operation (HTTP "+str(error.code)+")") from None
    def json(self,path,method="GET",data=None,missing=False):
        response=self.request(method,path,None if data is None else canonical(data),missing)
        if response is None:return None
        with response: return json.load(response)
    def items(self,path):
        items=[]
        for page in range(1,1001):
            response=self.request("GET",path+("&" if "?" in path else "?")+"per_page=100&page="+str(page))
            with response:
                rows=json.load(response);require(isinstance(rows,list),"Invalid GitLab list")
                items.extend(rows)
                if not response.headers.get("X-Next-Page"):return items
        raise ValueError("GitLab provenance pagination limit exceeded")

def context(env):
    tag=env.get("CI_COMMIT_TAG","");sha=env.get("CI_COMMIT_SHA","")
    require(VERSION.fullmatch(tag) and SHA.fullmatch(sha),"Formal releases require an exact vX.Y.Z tag and full source SHA")
    require(env.get("CI_PROJECT_PATH")=="gpuflow/gpuflow","Wrong Community project")
    require(env.get("CI_PIPELINE_SOURCE")=="push" and env.get("CI_COMMIT_REF_PROTECTED")=="true" and env.get("CI_COMMIT_BEFORE_SHA")=="0"*40,"Only the original protected new-tag push can publish")
    require(re.fullmatch(r"[1-9][0-9]*",env.get("CI_PIPELINE_ID","")),"Invalid pipeline ID")
    return {"version":tag,"source_commit":sha,"pipeline_id":int(env["CI_PIPELINE_ID"]),"project_id":int(env["CI_PROJECT_ID"])}

def remote_object(env,ctx):
    require(run("git","rev-parse","HEAD")==ctx["source_commit"],"Checkout source changed")
    token=env.get("GITLAB_RELEASE_TOKEN") or env.get("CI_JOB_TOKEN")
    user="oauth2" if env.get("GITLAB_RELEASE_TOKEN") else "gitlab-ci-token"
    child={k:v for k,v in env.items() if not k.startswith("GIT_TRACE") and k!="GIT_CURL_VERBOSE"}
    child.update(GIT_TERMINAL_PROMPT="0",GIT_CONFIG_COUNT="1",GIT_CONFIG_KEY_0="http.extraHeader",GIT_CONFIG_VALUE_0="Authorization: Basic "+base64.b64encode((user+":"+token).encode()).decode())
    ref="refs/tags/"+ctx["version"]
    output=run("git","-c","http.followRedirects=false","-c","http.proxy=","ls-remote","--tags",env["CI_SERVER_URL"].rstrip("/")+"/gpuflow/gpuflow.git",ref,ref+"^{}",env=child)
    rows={}
    for line in output.splitlines():
        parts=line.split("\t");require(len(parts)==2 and SHA.fullmatch(parts[0]) and parts[1] in (ref,ref+"^{}") and parts[1] not in rows,"Invalid remote tag snapshot");rows[parts[1]]=parts[0]
    require(ref in rows and rows.get(ref+"^{}",rows.get(ref))==ctx["source_commit"],"Remote tag moved or disappeared")
    return rows[ref]

def provenance(env,api,snapshot=remote_object):
    ctx=context(env)
    tag=api.json("/repository/tags/"+ctx["version"])
    require(tag.get("protected") is True and tag.get("name")==ctx["version"] and tag.get("commit",{}).get("id")==ctx["source_commit"],"Protected tag does not match source")
    pipeline=api.json("/pipelines/"+str(ctx["pipeline_id"]))
    require(pipeline.get("sha")==ctx["source_commit"] and pipeline.get("ref")==ctx["version"] and pipeline.get("source")=="push","Pipeline source mismatch")
    pipelines=api.items("/pipelines?"+urllib.parse.urlencode({"ref":ctx["version"],"source":"push"}))
    require(len(pipelines)==1 and pipelines[0].get("id")==ctx["pipeline_id"],"Tag has multiple push pipelines; reuse is forbidden")
    ctx["tag_object_sha"]=snapshot(env,ctx)
    events=[event for event in api.items("/events?sort=asc") if event.get("push_data",{}).get("ref_type")=="tag" and event["push_data"].get("ref")==ctx["version"]]
    require(len(events)==1,"A unique tag creation event is required")
    event=events[0];push=event["push_data"]
    require(event.get("project_id")==ctx["project_id"] and push.get("action")=="created" and push.get("commit_from") in (None,"0"*40) and push.get("commit_to") in (ctx["source_commit"],ctx["tag_object_sha"]),"Tag was moved, recreated or has missing history")
    def stamp(value):
        parsed=datetime.fromisoformat(value.replace("Z","+00:00"))
        require(parsed.tzinfo is not None,"Tag timestamps require timezone")
        return parsed
    require(abs((stamp(event["created_at"])-stamp(pipeline["created_at"])).total_seconds())<=300,"Tag creation does not belong to this pipeline")
    return ctx

def check(env,api):
    actual=context(env)
    actual["tag_object_sha"]=remote_object(env,actual)
    require(json.loads(GUARD.read_text())==actual,"Frozen release context changed")
    # Build jobs need only read_repository. Events/Pipelines API credentials
    # remain scoped to the protected publication environment.
    if env.get("CI_JOB_NAME")=="publish-version":
        require(provenance(env,api)==actual,"Release provenance changed")
    return actual

def assets(directory):
    require(directory.is_dir() and not directory.is_symlink(),"Invalid asset directory")
    result={}
    for path in sorted(directory.iterdir()):
        require(path.is_file() and not path.is_symlink() and re.fullmatch(r"[A-Za-z0-9_.-]+",path.name),"Unsafe release asset")
        if path.name in ("RELEASE-EVIDENCE.json","RELEASE-EVIDENCE.json.sigstore.json"):continue
        result[path.name]={"sha256":hash_file(path),"size":path.stat().st_size}
    return result

def verify_assets(directory,ctx):
    required={"gpuflow-linux-amd64.tar.gz","gpuflow-linux-arm64.tar.gz","gpuflow-windows-amd64.zip","gpuflow-deployment-"+ctx["version"]+".tar.gz"}
    required|={"gpuflow-offline-"+ctx["version"]+"-linux-"+arch+".tar.gz" for arch in ("amd64","arm64")}
    required|={name+".sigstore.json" for name in required}|{"checksums.txt","checksums.txt.sigstore.json","cosign.pub","trusted_root.json"}
    actual=assets(directory);require(set(actual)==required,"Formal Community asset set is incomplete or unexpected")
    trusted=os.environ.get("COSIGN_PUBLIC_KEY_FILE","")
    require(trusted and pathlib.Path(trusted).read_bytes()==(directory/"cosign.pub").read_bytes(),"An independently configured Community public key must match the artifacts")
    trust_root=os.environ.get("SIGSTORE_TRUSTED_ROOT_FILE","")
    require(trust_root and pathlib.Path(trust_root).read_bytes()==(directory/"trusted_root.json").read_bytes(),"Transparency trust root differs from protected configuration")
    for name in sorted(required):
        if name.endswith((".tar.gz",".zip")) or name=="checksums.txt":
            run("cosign","verify-blob","--offline","--trusted-root",str(directory/"trusted_root.json"),"--key",trusted,"--bundle",str(directory/(name+".sigstore.json")),str(directory/name))
    sums={}
    for line in (directory/"checksums.txt").read_text().splitlines():
        match=re.fullmatch(r"([a-f0-9]{64})  (?:\./)?([A-Za-z0-9_.-]+)",line)
        require(match and match[2] not in sums,"Invalid or duplicate signed checksum row")
        sums[match[2]]=match[1]
    wanted={name:row["sha256"] for name,row in actual.items() if name.endswith((".tar.gz",".zip")) or name=="trusted_root.json"}
    require(sums==wanted,"Signed checksums differ from exact archive bytes")
    return actual

def main():
    p=argparse.ArgumentParser();p.add_argument("command",choices=("guard","check","record","publish"));p.add_argument("--assets-dir",default="gitlab-artifacts/full");p.add_argument("--app");p.add_argument("--probe");a=p.parse_args()
    env=dict(os.environ);api=API(env)
    if a.command in ("guard","publish"):
        require(env.get("GITLAB_RELEASE_TOKEN"),"Protected release API token is required for tag history verification")
    if a.command=="guard":
        require(env.get("CI_JOB_NAME")=="formal-tag-guard","Wrong guard job")
        ctx=provenance(env,api)
        require(api.json("/releases/"+ctx["version"],missing=True) is None,"Version release already exists")
        GUARD.parent.mkdir(exist_ok=True);GUARD.write_bytes(canonical(ctx));return
    ctx=check(env,api)
    if a.command=="check":return
    directory=(ROOT/a.assets_dir).resolve()
    if a.command=="record":
        require(env.get("CI_JOB_NAME")=="full-signed-build","Only the verified build may record evidence")
        require(all(value and re.fullmatch(r"[A-Za-z0-9._:/-]+@sha256:[a-f0-9]{64}",value) for value in (a.app,a.probe)),"Immutable image references required")
        evidence={"schema":1,"context":ctx,"build_job_id":int(env["CI_JOB_ID"]),"app":a.app,"probe":a.probe,"assets":verify_assets(directory,ctx),"gates":{"offline_amd64":"passed","arm64_package":"verified"}}
        (directory/"RELEASE-EVIDENCE.json").write_bytes(canonical(evidence));return
    require(env.get("CI_JOB_NAME")=="publish-version","Wrong publication job")
    evidence=json.loads((directory/"RELEASE-EVIDENCE.json").read_text())
    require(evidence.get("context")==ctx and evidence.get("assets")==verify_assets(directory,ctx) and evidence.get("gates")=={"offline_amd64":"passed","arm64_package":"verified"},"Evidence differs from verified assets")
    run("cosign","verify-blob","--offline","--trusted-root",str(directory/"trusted_root.json"),"--key",env["COSIGN_PUBLIC_KEY_FILE"],"--bundle",str(directory/"RELEASE-EVIDENCE.json.sigstore.json"),str(directory/"RELEASE-EVIDENCE.json"))
    job=api.json("/jobs/"+str(evidence["build_job_id"]))
    require(job.get("name")=="full-signed-build" and job.get("status")=="success" and job.get("pipeline",{}).get("id")==ctx["pipeline_id"] and job.get("commit",{}).get("id")==ctx["source_commit"],"Upstream build is not successful for this source/pipeline")
    release_path="/releases/"+ctx["version"]
    require(api.json(release_path,missing=True) is None,"Published release cannot be replaced")
    links=[];present=set()
    # Inspect the complete destination before any upload. A conflicting asset
    # must not produce a partially updated version coordinate.
    for path in sorted(directory.iterdir()):
        coordinate="/packages/generic/gpuflow-community-release/"+ctx["version"]+"/"+path.name
        existing=api.request("GET",coordinate,missing=True)
        if existing is not None:
            with existing: require(hashlib.file_digest(existing,"sha256").hexdigest()==hash_file(path),"Existing asset bytes differ; refusing overwrite")
            present.add(path.name)
    for path in sorted(directory.iterdir()):
        check(env,api)
        coordinate="/packages/generic/gpuflow-community-release/"+ctx["version"]+"/"+path.name
        if path.name not in present:
            with path.open("rb") as stream:
                with api.request("PUT",coordinate,stream,length=path.stat().st_size):pass
        with api.request("GET",coordinate) as stream:require(hashlib.file_digest(stream,"sha256").hexdigest()==hash_file(path),"Uploaded asset checksum mismatch")
        links.append({"name":path.name,"url":api.base+coordinate,"link_type":"package"})
    # Images are signed candidates until all final package gates passed. Image
    # tags are promoted by a checked, immutable digest without another build.
    for key,suffix in (("app",""),("probe","/probe")):
        ref=evidence[key];require(ref.startswith(env["CI_REGISTRY_IMAGE"]+suffix+"@sha256:"),"Foreign release image")
        run("cosign","verify","--allow-http-registry","--key",env["COSIGN_PUBLIC_KEY_FILE"],ref)
        target=env["CI_REGISTRY_IMAGE"]+suffix+":"+ctx["version"]
        run("bash",str(ROOT/"scripts/gitlab-promote-image.sh"),ref,target)
    check(env,api)
    api.json("/releases",method="POST",data={"tag_name":ctx["version"],"name":"GPUFlow Community "+ctx["version"],"description":"Signed Community delivery. Source `"+ctx["source_commit"]+"`. Evidence and independently verifiable checksums are attached.","assets":{"links":links}})
    require(api.json(release_path).get("tag_name")==ctx["version"],"Release read-back failed")

if __name__=="__main__":
    try:main()
    except Exception as error:print("Community release blocked: "+str(error),file=sys.stderr);sys.exit(1)
