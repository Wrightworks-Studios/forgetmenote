"""Read-only deployment export and backup health checks for AWS Lambda.

AWS Backup owns snapshot schedules. This worker has no Kubernetes
write permission and no permission to start, stop or delete backups.
"""
import base64
import datetime as dt
import hashlib
import json
import os
import ssl
import urllib.error
import urllib.request


def backup(runtime, run_id):
    key = runtime.export_config(run_id, runtime.get_workload())
    health = runtime.backup_health()
    receipt = {"configuration": key, **health}
    runtime.put_json(key.replace("config.json", "receipt.json"), receipt)
    runtime.publish_health(health["healthy"])
    return receipt


class AWSRuntime:
    def __init__(self):
        import boto3
        self.session = boto3.Session()
        self.region = os.environ["AWS_REGION"]
        self.cluster = os.environ["CLUSTER"]
        self.namespace = os.environ["NAMESPACE"]
        self.instance = os.environ["INSTANCE"]
        self.workload = os.environ["WORKLOAD"]
        self.claim = os.environ["CLAIM"]
        self.volume = os.environ["VOLUME"]
        self.vault = os.environ["VAULT"]
        self.bucket = os.environ["BUCKET"]
        self.api = f"/apis/apps/v1/namespaces/{self.namespace}/statefulsets/{self.workload}"
        self.core = f"/api/v1/namespaces/{self.namespace}"
        self.aws_backup = self.session.client("backup")
        cluster = self.session.client("eks").describe_cluster(name=self.cluster)["cluster"]
        self.endpoint = cluster["endpoint"]
        self.tls = ssl.create_default_context(cadata=base64.b64decode(cluster["certificateAuthority"]["data"]).decode())

    def token(self):
        from botocore.signers import RequestSigner
        sts = self.session.client("sts")
        signer = RequestSigner(sts.meta.service_model.service_id, self.region, "sts", "v4",
                               self.session.get_credentials(), self.session.events)
        url = signer.generate_presigned_url({
            "method": "GET", "url": sts.meta.endpoint_url + "/?Action=GetCallerIdentity&Version=2011-06-15",
            "body": {}, "headers": {"x-k8s-aws-id": self.cluster}, "context": {}},
            region_name=self.region, expires_in=60, operation_name="")
        return "k8s-aws-v1." + base64.urlsafe_b64encode(url.encode()).decode().rstrip("=")

    def kube(self, method, path):
        if method != "GET":
            raise ValueError("backup worker only reads Kubernetes resources")
        request = urllib.request.Request(self.endpoint + path,
            headers={"Authorization": "Bearer " + self.token(), "Accept": "application/json"})
        try:
            with urllib.request.urlopen(request, context=self.tls, timeout=20) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            # Secret response bodies and bearer tokens must never enter logs.
            raise RuntimeError(f"Kubernetes {method} {path} returned HTTP {error.code}") from None

    def get_workload(self):
        return self.kube("GET", self.api)

    def put_json(self, key, value):
        body = json.dumps(value, separators=(",", ":"), sort_keys=True).encode()
        now = dt.datetime.now(dt.timezone.utc)
        retain = now + dt.timedelta(days=14)
        self.session.client("s3").put_object(
            Bucket=self.bucket, Key=key, Body=body, ContentType="application/json",
            ServerSideEncryption="aws:kms", SSEKMSKeyId=os.environ["KMS_KEY"],
            ChecksumSHA256=base64.b64encode(hashlib.sha256(body).digest()).decode(),
            ObjectLockMode="GOVERNANCE", ObjectLockRetainUntilDate=retain)

    def export_config(self, run_id, workload):
        pvc = self.kube("GET", self.core + "/persistentvolumeclaims/" + self.claim)
        pv = self.kube("GET", "/api/v1/persistentvolumes/" + pvc["spec"]["volumeName"])
        if pv["spec"].get("csi", {}).get("volumeHandle") != self.volume:
            raise RuntimeError("PVC no longer references the configured backup volume")
        if pv["spec"]["persistentVolumeReclaimPolicy"] != "Retain":
            raise RuntimeError("data PV must use Retain")
        # Gateways keep no disk of their own; their configuration restores them.
        deployments = [self.kube("GET", f"/apis/apps/v1/namespaces/{self.namespace}/deployments/{name}")
                       for name in os.environ.get("DEPLOYMENTS", "").split()]
        secrets, configs = set(), set()
        for item in [workload, *deployments]:
            self.references(item["spec"]["template"]["spec"], secrets, configs)
        objects = [workload, *deployments, pvc, pv]
        for plural, names in [("secrets", secrets), ("configmaps", configs),
                              ("services", os.environ.get("SERVICES", "").split())]:
            objects.extend(self.kube("GET", self.core + f"/{plural}/{name}") for name in sorted(names))
        for name in os.environ.get("INGRESSES", "").split():
            objects.append(self.kube("GET", f"/apis/networking.k8s.io/v1/namespaces/{self.namespace}/ingresses/{name}"))
        key = "runs/" + hashlib.sha256(run_id.encode()).hexdigest() + "/config.json"
        self.put_json(key, {"version": 1, "run_id": run_id, "volume": self.volume,
                            "cluster": self.cluster, "objects": objects})
        return key

    def references(self, spec, secrets, configs):
        """Add the secrets and config maps a pod spec uses, refusing a volume
        the backup does not cover."""
        secrets.update(s["name"] for s in spec.get("imagePullSecrets", []))
        for volume in spec.get("volumes", []):
            if ("hostPath" in volume or "csi" in volume or
                    volume.get("persistentVolumeClaim", {}).get("claimName", self.claim) != self.claim):
                raise RuntimeError("workload has an uncovered persistent volume")
            if "secret" in volume:
                secrets.add(volume["secret"]["secretName"])
            if "configMap" in volume:
                configs.add(volume["configMap"]["name"])
            if "projected" in volume:
                sources = volume["projected"].get("sources", [])
                if not sources:
                    raise RuntimeError("projected volume has no sources")
                for source in sources:
                    if len(source) != 1 or not set(source) <= {"secret", "configMap", "serviceAccountToken", "downwardAPI"}:
                        raise RuntimeError("projected volume has an uncovered source")
                    if "secret" in source:
                        secrets.add(source["secret"]["name"])
                    if "configMap" in source:
                        configs.add(source["configMap"]["name"])
                    # Kubernetes regenerates tokens and downward API files from the saved pod spec.
        for container in spec.get("containers", []) + spec.get("initContainers", []):
            for ref in container.get("envFrom", []):
                if "secretRef" in ref:
                    secrets.add(ref["secretRef"]["name"])
                if "configMapRef" in ref:
                    configs.add(ref["configMapRef"]["name"])
            for env in container.get("env", []):
                ref = env.get("valueFrom", {})
                if "secretKeyRef" in ref:
                    secrets.add(ref["secretKeyRef"]["name"])
                if "configMapKeyRef" in ref:
                    configs.add(ref["configMapKeyRef"]["name"])

    def backup_health(self):
        now = dt.datetime.now(dt.timezone.utc)
        volume = f"arn:aws:ec2:{self.region}:{os.environ['ACCOUNT']}:volume/{self.volume}"
        jobs = {}
        for arn in os.environ.get("RESOURCES", "").split() or [volume]:
            jobs[arn] = []
            for page in self.aws_backup.get_paginator("list_backup_jobs").paginate(
                    ByResourceArn=arn, ByCreatedAfter=now - dt.timedelta(hours=48), ByBackupVaultName=self.vault):
                jobs[arn].extend(page["BackupJobs"])
        return assess_resources(jobs, now)

    def publish_health(self, healthy):
        self.session.client("cloudwatch").put_metric_data(
            Namespace="ForgetMeNote/Recovery", MetricData=[{
                "MetricName": "Healthy", "Dimensions": [{"Name": "Instance", "Value": self.instance}],
                "Value": int(healthy), "Unit": "Count"}])


def assess_jobs(backups, now):
    # Creation time measures the age of the data, not how recently upload ended.
    primary_fresh = any(j["State"] == "COMPLETED" and
                        j["CreationDate"] >= now - dt.timedelta(hours=30) for j in backups)
    failures = [{"job": j["BackupJobId"], "state": j["State"]}
                for j in backups if j["State"] in ("FAILED", "EXPIRED", "ABORTED")
                and j["CreationDate"] >= now - dt.timedelta(hours=24)]
    return {"checked_at": now.isoformat(), "primary_fresh": primary_fresh,
            "recent_failures": failures, "healthy": primary_fresh and not failures}


def assess_resources(jobs, now):
    """An instance is healthy only while every protected resource is."""
    health = {arn: assess_jobs(backups, now) for arn, backups in jobs.items()}
    return {"checked_at": now.isoformat(), "primary_fresh": all(h["primary_fresh"] for h in health.values()),
            "stale": [arn for arn, h in health.items() if not h["primary_fresh"]],
            "recent_failures": [{**failure, "resource": arn} for arn, h in health.items() for failure in h["recent_failures"]],
            "healthy": all(h["healthy"] for h in health.values())}


def handler(event, context):
    return backup(AWSRuntime(), event.get("id", context.aws_request_id))
