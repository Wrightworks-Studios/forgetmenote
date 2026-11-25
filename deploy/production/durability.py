#!/usr/bin/env python3
"""Render CloudFormation and narrow Kubernetes RBAC for one EBS-backed instance.

Uses only Python's standard library. Rendering never contacts or changes AWS.
Apply the template in the same region as the instance.
"""
import argparse
import json
from pathlib import Path
import re


REF = lambda name: {"Ref": name}
ATT = lambda name, key="Arn": {"Fn::GetAtt": [name, key]}
SUB = lambda value: {"Fn::Sub": value}


def resource(kind, properties, *, retain=False, **extra):
    value = {"Type": kind, "Properties": properties, **extra}
    if retain:
        value.update(DeletionPolicy="Retain", UpdateReplacePolicy="Retain")
    return value


def policy(statements):
    return {"Version": "2012-10-17", "Statement": statements}


def allow(actions, resources):
    return {"Effect": "Allow", "Action": actions, "Resource": resources}


def trust(service):
    return policy([{"Effect": "Allow", "Principal": {"Service": service}, "Action": "sts:AssumeRole"}])


def storage(config):
    name = config["name"]
    return {
        "Key": resource("AWS::KMS::Key", {
            "Description": f"{name} recovery data and deployment assets", "EnableKeyRotation": True,
            "KeyPolicy": policy([{"Effect": "Allow", "Principal": {"AWS": SUB("arn:${AWS::Partition}:iam::${AWS::AccountId}:root")},
                                  "Action": "kms:*", "Resource": "*"}])}, retain=True),
        "KeyAlias": resource("AWS::KMS::Alias", {"AliasName": f"alias/{name}-recovery", "TargetKeyId": REF("Key")}),
        "Vault": resource("AWS::Backup::BackupVault", {
            "BackupVaultName": name, "EncryptionKeyArn": ATT("Key"),
            "LockConfiguration": {"MinRetentionDays": 7, "MaxRetentionDays": 365},
            "BackupVaultTags": {"Application": name}}, retain=True),
        "Bucket": resource("AWS::S3::Bucket", {
            "BucketName": SUB(name + "-recovery-${AWS::AccountId}-${AWS::Region}"),
            "VersioningConfiguration": {"Status": "Enabled"}, "ObjectLockEnabled": True,
            "ObjectLockConfiguration": {"ObjectLockEnabled": "Enabled", "Rule": {
                "DefaultRetention": {"Mode": "GOVERNANCE", "Days": 14}}},
            "BucketEncryption": {"ServerSideEncryptionConfiguration": [{
                "BucketKeyEnabled": True, "ServerSideEncryptionByDefault": {
                    "SSEAlgorithm": "aws:kms", "KMSMasterKeyID": ATT("Key")}}]},
            "PublicAccessBlockConfiguration": {"BlockPublicAcls": True, "BlockPublicPolicy": True,
                "IgnorePublicAcls": True, "RestrictPublicBuckets": True},
            "OwnershipControls": {"Rules": [{"ObjectOwnership": "BucketOwnerEnforced"}]},
            "LifecycleConfiguration": {"Rules": [{"Id": "ExpireOldRuns", "Status": "Enabled", "Prefix": "runs/",
                "ExpirationInDays": 35, "NoncurrentVersionExpiration": {"NoncurrentDays": 35}},
                {"Id": "AbandonedUploads", "Status": "Enabled", "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 7}}]}}, retain=True),
        "BucketPolicy": resource("AWS::S3::BucketPolicy", {"Bucket": REF("Bucket"), "PolicyDocument": policy([
            {"Effect": "Deny", "Principal": "*", "Action": "s3:*", "Resource": [ATT("Bucket"), SUB("${Bucket.Arn}/*")],
             "Condition": {"Bool": {"aws:SecureTransport": "false"}}}])})}


def outputs():
    return {"VaultArn": {"Value": ATT("Vault", "BackupVaultArn")}, "Bucket": {"Value": REF("Bucket")},
            "KmsKeyArn": {"Value": ATT("Key")}}


def protected(config):
    """The volume, and on the Postgres stack its database and blob bucket: each
    holds state a restore needs, so each is backed up and checked for freshness."""
    arns = [f"arn:aws:ec2:{config['region']}:{config['account']}:volume/{config['volume']}"]
    if config.get("database_instance"):
        arns.append(f"arn:aws:rds:{config['region']}:{config['account']}:db:{config['database_instance']}")
    if config.get("blob_bucket"):
        arns.append(f"arn:aws:s3:::{config['blob_bucket']}")
    return arns


def workload(config):
    """The StatefulSet whose volume is protected. The name stays the stack's,
    since it names the locked vault and key; after a move onto Postgres the
    workload is the owner."""
    return config.get("workload", config["name"])


def primary_template(config):
    name = config["name"]
    cluster_arn = f"arn:aws:eks:{config['region']}:{config['account']}:cluster/{config['cluster']}"
    res = storage(config)
    backup_policies = ["arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForBackup"]
    restore_policies = ["arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForRestores"]
    if config.get("blob_bucket"):
        backup_policies.append("arn:aws:iam::aws:policy/AWSBackupServiceRolePolicyForS3Backup")
        restore_policies.append("arn:aws:iam::aws:policy/AWSBackupServiceRolePolicyForS3Restore")
    res["BackupRole"] = resource("AWS::IAM::Role", {
        "RoleName": name + "-backup-service", "AssumeRolePolicyDocument": trust("backup.amazonaws.com"),
        "ManagedPolicyArns": backup_policies})
    res["RestoreRole"] = resource("AWS::IAM::Role", {
        "RoleName": name + "-restore-service", "AssumeRolePolicyDocument": trust("backup.amazonaws.com"),
        "ManagedPolicyArns": restore_policies})
    rules = [{"RuleName": "Daily", "TargetBackupVault": REF("Vault"),
        "ScheduleExpression": "cron(0 9 * * ? *)", "StartWindowMinutes": 60,
        "CompletionWindowMinutes": 120, "Lifecycle": {"DeleteAfterDays": 14},
        "RecoveryPointTags": {"Application": name, "Consistency": "crash"}}]
    # Keep the deployed stack's logical ID when changing its schedule.
    res["HourlyPlan"] = resource("AWS::Backup::BackupPlan", {"BackupPlan": {
        "BackupPlanName": name + "-scheduled", "BackupPlanRule": rules},
        "BackupPlanTags": {"Application": name}})
    res["Selection"] = resource("AWS::Backup::BackupSelection", {"BackupPlanId": REF("HourlyPlan"),
        "BackupSelection": {"SelectionName": name, "IamRoleArn": ATT("BackupRole"), "Resources": protected(config)}})
    res["WorkerRole"] = resource("AWS::IAM::Role", {"RoleName": name + "-backup-worker",
        "AssumeRolePolicyDocument": trust("lambda.amazonaws.com"), "Policies": [{
            "PolicyName": "InstanceBackup", "PolicyDocument": policy([
                allow(["logs:CreateLogStream", "logs:PutLogEvents"], ATT("WorkerLog")),
                allow("eks:DescribeCluster", cluster_arn),
                allow(["s3:PutObject", "s3:PutObjectRetention"], SUB("${Bucket.Arn}/runs/*")),
                allow(["kms:GenerateDataKey", "kms:Encrypt"], ATT("Key")),
                allow("backup:ListBackupJobs", "*"),
                {**allow("cloudwatch:PutMetricData", "*"),
                 "Condition": {"StringEquals": {"cloudwatch:namespace": "ForgetMeNote/Recovery"}}}
            ])}]})
    env = {"ACCOUNT": config["account"], "CLUSTER": config["cluster"], "NAMESPACE": config["namespace"],
        "INSTANCE": name, "WORKLOAD": workload(config), "CLAIM": config["claim"], "VOLUME": config["volume"], "VAULT": REF("Vault"),
        "BUCKET": REF("Bucket"), "KMS_KEY": ATT("Key"),
        "SERVICES": " ".join(config["services"]), "INGRESSES": " ".join(config.get("ingresses", [])),
        "DEPLOYMENTS": " ".join(config.get("deployments", [])), "RESOURCES": " ".join(protected(config))}
    res["WorkerLog"] = resource("AWS::Logs::LogGroup", {
        "LogGroupName": f"/aws/lambda/{name}-backup", "RetentionInDays": 30})
    res["Worker"] = resource("AWS::Lambda::Function", {"FunctionName": name + "-backup", "Runtime": "python3.12",
        "Handler": "index.handler", "Role": ATT("WorkerRole"), "Timeout": 120, "MemorySize": 256,
        "ReservedConcurrentExecutions": 1, "Environment": {"Variables": env},
        "Code": {"ZipFile": Path(__file__).with_name("durability_worker.py").read_text()}})
    res["ClusterAccess"] = resource("AWS::EKS::AccessEntry", {"ClusterName": config["cluster"],
        "PrincipalArn": ATT("WorkerRole"), "Type": "STANDARD", "KubernetesGroups": [name + "-backup"]})
    res["Schedule"] = resource("AWS::Events::Rule", {"Name": name + "-configuration-backup",
        "ScheduleExpression": "cron(15 12 * * ? *)", "State": REF("ExportState"),
        "Targets": [{"Id": "Export", "Arn": ATT("Worker"),
                     "RetryPolicy": {"MaximumRetryAttempts": 0, "MaximumEventAgeInSeconds": 3600}}]})
    res["InvokePermission"] = resource("AWS::Lambda::Permission", {
        "Action": "lambda:InvokeFunction", "FunctionName": REF("Worker"),
        "Principal": "events.amazonaws.com", "SourceArn": ATT("Schedule")})
    res["AsyncPolicy"] = resource("AWS::Lambda::EventInvokeConfig", {
        "FunctionName": REF("Worker"), "Qualifier": "$LATEST", "MaximumRetryAttempts": 0,
        "MaximumEventAgeInSeconds": 3600})
    for logical, namespace, metric, dimensions, period, threshold, comparison, missing in [
            ("FailureAlarm", "AWS/Lambda", "Errors", [{"Name": "FunctionName", "Value": REF("Worker")}],
             300, 1, "GreaterThanOrEqualToThreshold", "notBreaching"),
            ("FreshnessAlarm", "ForgetMeNote/Recovery", "Healthy", [{"Name": "Instance", "Value": name}],
             129600, 1, "LessThanThreshold", "breaching")]:
        res[logical] = resource("AWS::CloudWatch::Alarm", {"AlarmName": name + "-backup-" + logical,
            "AlarmDescription": "Check configuration export and recent AWS Backup jobs for this instance's volume, database and blob bucket.",
            "Namespace": namespace, "MetricName": metric, "Dimensions": dimensions,
            "Statistic": "Minimum" if metric == "Healthy" else "Sum", "Period": period,
            "EvaluationPeriods": 1, "Threshold": threshold,
            "ComparisonOperator": comparison, "TreatMissingData": missing,
            "AlarmActions": [], "OKActions": [], "InsufficientDataActions": []})
    out = outputs()
    out.update({"WorkerArn": {"Value": ATT("Worker")}, "WorkerRoleArn": {"Value": ATT("WorkerRole")},
                "BackupRoleArn": {"Value": ATT("BackupRole")}, "RestoreRoleArn": {"Value": ATT("RestoreRole")}})
    return {"AWSTemplateFormatVersion": "2010-09-09", "Description": "ForgetMeNote backup and recovery for one instance",
        "Parameters": {"ExportState": {"Type": "String", "Default": "DISABLED", "AllowedValues": ["DISABLED", "ENABLED"]}},
        "Resources": res, "Outputs": out}


def rbac(config):
    name = config["name"] + "-backup"
    namespace = config["namespace"]
    rule = lambda group, resources, verbs, names=None: dict(apiGroups=[group], resources=resources, verbs=verbs,
                                                            **({"resourceNames": names} if names else {}))
    rules = [rule("apps", ["statefulsets"], ["get"], [workload(config)]),
             rule("", ["persistentvolumeclaims"], ["get"], [config["claim"]]),
             rule("", ["secrets"], ["get"], config["secrets"]),
             rule("", ["services"], ["get"], config["services"])]
    if config.get("configmaps"):
        rules.append(rule("", ["configmaps"], ["get"], config["configmaps"]))
    if config.get("ingresses"):
        rules.append(rule("networking.k8s.io", ["ingresses"], ["get"], config["ingresses"]))
    if config.get("deployments"):
        rules.append(rule("apps", ["deployments"], ["get"], config["deployments"]))
    objects = [{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
                "metadata": {"name": name, "namespace": namespace}, "rules": rules},
               {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
                "metadata": {"name": name}, "rules": [rule("", ["persistentvolumes"], ["get"], [config["pv"]])]}]
    for kind in ["Role", "ClusterRole"]:
        objects.append({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": kind + "Binding",
            "metadata": {"name": name, **({"namespace": namespace} if kind == "Role" else {})},
            "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name},
            "subjects": [{"kind": "Group", "name": name, "apiGroup": "rbac.authorization.k8s.io"}]})
    return {"apiVersion": "v1", "kind": "List", "items": objects}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    if not re.fullmatch(r"[a-z][a-z0-9-]{2,35}", config["name"]):
        parser.error("name must be a short lowercase service name")
    if not re.fullmatch(r"[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?", workload(config)):
        parser.error("workload must be a Kubernetes StatefulSet name")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=True)
    for name, value in [("primary.json", primary_template(config)),
                         ("rbac.json", rbac(config))]:
        path = args.output / name
        path.write_text(json.dumps(value, indent=2) + "\n")
        path.chmod(0o600)
    print("Rendered primary.json and rbac.json; no infrastructure changed.")


if __name__ == "__main__":
    main()
