#!/usr/bin/env python3
"""Hermetic channel checks; no keys, network, registry or real publication."""
import copy, importlib.util, io, json, os, pathlib, tempfile, unittest
from unittest import mock
spec=importlib.util.spec_from_file_location("release",pathlib.Path(__file__).with_name("gitlab-release.py"))
release=importlib.util.module_from_spec(spec);spec.loader.exec_module(release)
SHA="a"*40;TAG="v20.30.40"

class FakeAPI:
    base="http://gitlab.gpuflow.test:8088/api/v4/projects/7"
    def __init__(self):
        self.tag={"name":TAG,"protected":True,"commit":{"id":SHA}}
        self.pipeline={"id":9,"sha":SHA,"ref":TAG,"source":"push","created_at":"2026-09-19T00:00:00Z"}
        self.pipelines=[self.pipeline]
        self.events=[{"project_id":7,"created_at":self.pipeline["created_at"],"push_data":{"ref_type":"tag","ref":TAG,"action":"created","commit_from":None,"commit_to":SHA}}]
        self.job={"name":"full-signed-build","status":"success","pipeline":{"id":9},"commit":{"id":SHA}}
        self.packages={};self.mutations=[];self.published=None
    def json(self,path,method="GET",data=None,missing=False):
        if method=="POST" and path=="/releases":self.mutations.append((method,path));self.published=data;return data
        if path.startswith("/repository/tags/"):return self.tag
        if path=="/pipelines/9":return self.pipeline
        if path=="/jobs/10":return self.job
        if path.startswith("/releases/"):return self.published
        raise AssertionError((method,path))
    def items(self,path):
        if path.startswith("/pipelines?"):return self.pipelines
        if path.startswith("/events?"):return self.events
        raise AssertionError(path)
    def request(self,method,path,data=None,missing=False,length=None):
        if method=="PUT":
            value=data.read();assert len(value)==length
            assert path not in self.packages
            self.packages[path]=value;self.mutations.append((method,path));return io.BytesIO(b'{}')
        if method=="GET":
            if path not in self.packages and missing:return None
            return io.BytesIO(self.packages[path])
        raise AssertionError((method,path))

class ReleaseTests(unittest.TestCase):
    def setUp(self):
        temporary=tempfile.TemporaryDirectory();self.addCleanup(temporary.cleanup);self.root=pathlib.Path(temporary.name)
        self.env={"CI_PROJECT_PATH":"gpuflow/gpuflow","CI_PROJECT_ID":"7","CI_PIPELINE_ID":"9","CI_COMMIT_TAG":TAG,"CI_COMMIT_SHA":SHA,"CI_PIPELINE_SOURCE":"push","CI_COMMIT_REF_PROTECTED":"true","CI_COMMIT_BEFORE_SHA":"0"*40,"CI_JOB_NAME":"publish-version","CI_JOB_ID":"11","CI_JOB_TOKEN":"synthetic-only","CI_SERVER_URL":"http://gitlab.gpuflow.test:8088","CI_REGISTRY_IMAGE":"gitlab.gpuflow.test:5055/gpuflow/gpuflow"}
        self.api=FakeAPI();self.calls=[]
        self.env["GITLAB_RELEASE_TOKEN"]="synthetic-api-token"
        self.directory=self.root/"assets";self.directory.mkdir()
        names=["gpuflow-linux-amd64.tar.gz","gpuflow-linux-arm64.tar.gz","gpuflow-windows-amd64.zip","gpuflow-deployment-"+TAG+".tar.gz"]+["gpuflow-offline-"+TAG+"-linux-"+a+".tar.gz" for a in ("amd64","arm64")]
        for name in names+[n+".sigstore.json" for n in names]+["checksums.txt","checksums.txt.sigstore.json","cosign.pub","trusted_root.json"]:(self.directory/name).write_bytes(("fixture-"+name).encode())
        (self.directory/"checksums.txt").write_text("".join(release.hash_file(self.directory/name)+"  ./"+name+"\n" for name in names+["trusted_root.json"]))
        for setting,name in (("COSIGN_PUBLIC_KEY_FILE","cosign.pub"),("SIGSTORE_TRUSTED_ROOT_FILE","trusted_root.json")):
            path=self.root/name;path.write_bytes((self.directory/name).read_bytes());self.env[setting]=str(path)
        self.context=release.provenance(self.env,self.api,lambda env,ctx:SHA)
        self.guard=self.root/"guard.json";self.guard.write_bytes(release.canonical(self.context))
        evidence={"schema":1,"context":self.context,"build_job_id":10,"app":self.env["CI_REGISTRY_IMAGE"]+"@sha256:"+"b"*64,"probe":self.env["CI_REGISTRY_IMAGE"]+"/probe@sha256:"+"c"*64,"assets":release.assets(self.directory),"gates":{"offline_amd64":"passed","arm64_package":"verified"}}
        (self.directory/"RELEASE-EVIDENCE.json").write_bytes(release.canonical(evidence));(self.directory/"RELEASE-EVIDENCE.json.sigstore.json").write_text("fixture-bundle")
    def command(self,*args,**kwargs):
        self.calls.append(args)
        if args[:3]==("git","rev-parse","HEAD"):return SHA
        if args[0]=="git":return SHA+"\trefs/tags/"+TAG
        if args[0] in ("cosign","bash"):return ""
        raise AssertionError(args)
    def invoke(self):
        with mock.patch.dict(os.environ,self.env,clear=True),mock.patch.object(release,"ROOT",self.root),mock.patch.object(release,"GUARD",self.guard),mock.patch.object(release,"API",return_value=self.api),mock.patch.object(release,"run",side_effect=self.command),mock.patch("sys.argv",["release","publish","--assets-dir","assets"]):release.main()
    def test_publishes_only_verified_assets_and_digests(self):
        self.invoke();self.assertEqual(len(self.api.packages),18);self.assertEqual(self.api.published["tag_name"],TAG)
        self.assertEqual(len([call for call in self.calls if call[0]=="bash"]),2)
    def test_failed_build_cannot_publish(self):
        self.api.job["status"]="failed"
        with self.assertRaisesRegex(ValueError,"Upstream"):self.invoke()
        self.assertFalse(self.api.mutations)
    def test_asset_tampering_cannot_publish(self):
        (self.directory/"gpuflow-linux-amd64.tar.gz").write_text("changed")
        with self.assertRaisesRegex(ValueError,"checksums differ"):self.invoke()
        self.assertFalse(self.api.mutations)
    def test_arm_package_required(self):
        (self.directory/("gpuflow-offline-"+TAG+"-linux-arm64.tar.gz")).unlink()
        with self.assertRaisesRegex(ValueError,"asset set"):self.invoke()
        self.assertFalse(self.api.mutations)
    def test_published_version_is_immutable(self):
        self.api.published={"tag_name":TAG}
        with self.assertRaisesRegex(ValueError,"cannot be replaced"):self.invoke()
        self.assertFalse(self.api.mutations)
    def test_existing_different_bytes_are_not_overwritten(self):
        self.api.packages["/packages/generic/gpuflow-community-release/"+TAG+"/RELEASE-EVIDENCE.json"]=b"different"
        with self.assertRaisesRegex(ValueError,"Existing asset bytes differ"):self.invoke()
        self.assertFalse(self.api.mutations)
    def test_untrusted_public_key_is_rejected(self):
        pathlib.Path(self.env["COSIGN_PUBLIC_KEY_FILE"]).write_text("other key")
        with self.assertRaisesRegex(ValueError,"public key"):self.invoke()
        self.assertFalse(self.api.mutations)
    def test_tag_context_refusals(self):
        for field,value in (("CI_COMMIT_TAG","v1.2.03"),("CI_PIPELINE_SOURCE","web"),("CI_COMMIT_REF_PROTECTED","false"),("CI_COMMIT_BEFORE_SHA",SHA),("CI_PROJECT_PATH","gpuflow/enterprise")):
            env=dict(self.env);env[field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):release.provenance(env,self.api,lambda env,ctx:SHA)
    def test_moved_recreated_and_duplicate_push_are_rejected(self):
        for mutation in ("moved","recreated","duplicate-pipeline","unprotected"):
            api=FakeAPI()
            if mutation=="moved":api.tag["commit"]["id"]="f"*40
            elif mutation=="recreated":api.events.append(copy.deepcopy(api.events[0]))
            elif mutation=="duplicate-pipeline":api.pipelines.append(copy.deepcopy(api.pipeline))
            else:api.tag["protected"]=False
            with self.subTest(mutation=mutation),self.assertRaises(ValueError):release.provenance(self.env,api,lambda env,ctx:SHA)

if __name__=="__main__":unittest.main()
