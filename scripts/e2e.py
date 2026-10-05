#!/usr/bin/env python3
"""Test the actual ExternalDNS image and official Helm chart in disposable kind.

Default: local API fixture. --live: use DNSCALE_E2E_DOMAIN, DNSCALE_E2E_ZONE_ID,
DNSCALE_E2E_TOKEN_FILE. Live mode creates records only under a random run prefix.
Requires docker, kind, kubectl, helm, and (for live DNS checks) dig on PATH.
"""
import argparse
import json
import os
import pathlib
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--live", action="store_true")
parser.add_argument("--image", default="dnscale-external-dns:e2e")
parser.add_argument("--keep-cluster", action="store_true", help="retain failed fixture cluster for debugging")
args = parser.parse_args()
if args.live and args.keep_cluster:
    parser.error("live runs always remove their credential-bearing cluster")
run_id = "edns-" + uuid.uuid4().hex[:8]
domain = run_id + "." + (os.environ["DNSCALE_E2E_DOMAIN"].rstrip(".") if args.live else "example.org")
zone_id = os.environ["DNSCALE_E2E_ZONE_ID"] if args.live else "11111111-1111-4111-8111-111111111111"
token = pathlib.Path(os.environ["DNSCALE_E2E_TOKEN_FILE"]).read_text().strip() if args.live else "fixture-token"
work = pathlib.Path(tempfile.mkdtemp(prefix="external-dns-e2e-"))
env = dict(os.environ, KUBECONFIG=str(work / "kubeconfig"))
namespace = "external-dns-test"
base_url = "https://api.dnscale.eu" if args.live else ""
forward = None
created_cluster = False
succeeded = False


def command(*cmd, data=None, capture=True, check=True):
    result = subprocess.run(cmd, input=data, text=True, env=env, stdout=subprocess.PIPE if capture else None, stderr=subprocess.PIPE if capture else None)
    if check and result.returncode:
        # Kubernetes logs can include live DNS names, so retain diagnostics locally.
        (work / "last-error.log").write_text((result.stdout or "") + (result.stderr or ""))
        raise RuntimeError(f"{cmd[0]} {cmd[1]} failed; diagnostics in temporary test directory")
    return result.stdout or ""


def kubectl(*cmd, **kwargs):
    return command("kubectl", "-n", namespace, *cmd, **kwargs)


def apply(obj):
    return kubectl("apply", "-f", "-", data=json.dumps(obj))


def request(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base_url + path, method=method, data=data, headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=20) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def records():
    result, offset = [], 0
    while True:
        page = request("GET", f"/v1/zones/{zone_id}/records?limit=100&offset={offset}")["data"]
        result.extend(page["records"])
        if not page["pagination"]["has_more"]:
            return [r for r in result if r["name"].rstrip(".").endswith("." + domain)]
        offset += len(page["records"])


def wait_for(predicate, description, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            print("PASS:", description, flush=True)
            return
        time.sleep(2)
    raise RuntimeError("timed out: " + description)


def values(name, typ):
    return sorted(r["content"].rstrip(".") for r in records() if r["name"].rstrip(".") == name + "." + domain and r["type"] == typ)


def release(policy="upsert-only", dry_run=False):
    extra = {"webhook-provider-url": "http://127.0.0.1:8888", "webhook-provider-read-timeout": "120s", "webhook-provider-write-timeout": "120s"}
    if dry_run:
        extra["dry-run"] = True
    overrides = {
        "sourceNamespace": namespace, "interval": "3s", "policy": policy,
        "domainFilters": [domain], "txtOwnerId": run_id, "extraArgs": extra,
        "provider": {"webhook": {"image": {"repository": args.image.rsplit(":", 1)[0], "tag": args.image.rsplit(":", 1)[1], "pullPolicy": "IfNotPresent"}, "env": [
            {"name": "DNSCALE_DRY_RUN", "value": "true" if dry_run else "false"},
            {"name": "DNSCALE_API_TOKEN_FILE", "value": "/etc/dnscale/token"},
            {"name": "DNSCALE_API_URL", "value": "https://api.dnscale.eu/v1" if args.live else "http://api-fixture:8000/v1"},
            {"name": "DNSCALE_ALLOW_HTTP", "value": "false" if args.live else "true"},
            {"name": "DNSCALE_DOMAIN_FILTER", "value": domain},
            {"name": "DNSCALE_ZONE_ID_FILTER", "value": zone_id},
            {"name": "DNSCALE_TXT_OWNER_ID", "value": run_id},
        ]}},
    }
    # Helm merges maps: explicitly null out the base dry-run key when enabling writes.
    if not dry_run:
        overrides["extraArgs"]["dry-run"] = False
    path = work / "values.json"
    path.write_text(json.dumps(overrides))
    command("helm", "upgrade", "--install", "external-dns", "external-dns", "--repo", "https://kubernetes-sigs.github.io/external-dns", "--version", "1.23.0", "--namespace", namespace, "-f", str(ROOT / "deploy/values.yaml"), "-f", str(path), "--wait", "--timeout", "180s")


def service(targets, ttl=300):
    apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": "test-service", "labels": {"external-dns": "enabled"}, "annotations": {"external-dns.kubernetes.io/hostname": "service." + domain, "external-dns.kubernetes.io/ttl": str(ttl)}}, "spec": {"type": "LoadBalancer", "ports": [{"port": 80}]}})
    kubectl("patch", "service", "test-service", "--subresource=status", "--type=merge", "-p", json.dumps({"status": {"loadBalancer": {"ingress": [{"hostname" if ":" not in value and not value[0].isdigit() else "ip": value} for value in targets]}}}))


def create_manual(name, typ, content):
    return request("POST", f"/v1/zones/{zone_id}/records", {"name": name + "." + domain, "type": typ, "content": content, "ttl": 300})["data"]["record"]


def public_dns(name, typ, expected):
    zone = os.environ["DNSCALE_E2E_DOMAIN"]
    servers = command("dig", "+short", "NS", zone).split()
    if not servers:
        raise RuntimeError("live zone has no public NS delegation")
    def check():
        return all(sorted(command("dig", "+short", "@" + server, name + "." + domain, typ).strip().splitlines()) == sorted(expected) for server in servers + ["1.1.1.1"])
    wait_for(check, "authoritative and recursive DNS agree", timeout=180)


try:
    command("kind", "create", "cluster", "--name", run_id, "--image", "kindest/node:v1.35.0", "--kubeconfig", env["KUBECONFIG"], "--wait", "120s")
    created_cluster = True
    if args.image.startswith("dnscale-external-dns:"):
        command("kind", "load", "docker-image", args.image, "--name", run_id)
    kubectl("create", "namespace", namespace)
    # Credentials go through stdin; never put the token in command arguments or logs.
    import base64
    apply({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "dnscale-api-token"}, "data": {"token": base64.b64encode(token.encode()).decode()}})
    if not args.live:
        kubectl("create", "configmap", "api-fixture", "--from-file=" + str(ROOT / "test/api_fixture.py"))
        apply({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "api-fixture"}, "spec": {"replicas": 1, "selector": {"matchLabels": {"app": "api-fixture"}}, "template": {"metadata": {"labels": {"app": "api-fixture"}}, "spec": {"containers": [{"name": "api", "image": "python:3.13-alpine", "command": ["python", "/test/api_fixture.py"], "ports": [{"containerPort": 8000}], "volumeMounts": [{"name": "test", "mountPath": "/test", "readOnly": True}]}], "volumes": [{"name": "test", "configMap": {"name": "api-fixture"}}]}}}})
        apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": "api-fixture"}, "spec": {"selector": {"app": "api-fixture"}, "ports": [{"port": 8000, "targetPort": 8000}]}})
        kubectl("rollout", "status", "deployment/api-fixture", "--timeout=120s")
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        forward = subprocess.Popen(["kubectl", "-n", namespace, "port-forward", "service/api-fixture", f"{port}:8000"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        base_url = f"http://127.0.0.1:{port}"
        time.sleep(2)
    manual = [create_manual("manual", "A", "192.0.2.90"), create_manual("_acme-challenge", "TXT", "independent-acme-token"), create_manual("foreign", "A", "192.0.2.91"), create_manual("edns-a.foreign", "TXT", "heritage=external-dns,external-dns/owner=another-controller")]
    service(["192.0.2.1", "192.0.2.2", "2001:db8::1"])
    apply({"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": {"name": "test-ingress", "labels": {"external-dns": "enabled"}}, "spec": {"rules": [{"host": "ingress." + domain, "http": {"paths": [{"path": "/", "pathType": "Prefix", "backend": {"service": {"name": "test-service", "port": {"number": 80}}}}]}}]}})
    kubectl("patch", "ingress", "test-ingress", "--subresource=status", "--type=merge", "-p", json.dumps({"status": {"loadBalancer": {"ingress": [{"hostname": "lb.example.net"}]}}}))
    release(dry_run=True)
    def dry_run_reconciled():
        logs = kubectl("logs", "deployment/external-dns", "-c", "webhook", "--tail=100")
        return "dry-run batch validated" in logs
    wait_for(dry_run_reconciled, "dry-run plans changes")
    assert not values("service", "A") and len(records()) == len(manual), "dry-run wrote DNS"
    print("PASS: dry-run preserves DNS", flush=True)
    release(policy="create-only")
    wait_for(lambda: values("service", "A") == ["192.0.2.1", "192.0.2.2"] and values("service", "AAAA") == ["2001:db8::1"] and values("ingress", "CNAME") == ["lb.example.net"], "Service A/AAAA and Ingress CNAME creation")
    assert len(values("edns-a.service", "TXT")) == 1 and len(values("edns-aaaa.service", "TXT")) == 1 and len(values("edns-cname.ingress", "TXT")) == 1
    if args.live:
        public_dns("service", "A", ["192.0.2.1", "192.0.2.2"])
        public_dns("service", "AAAA", ["2001:db8::1"])
        public_dns("ingress", "CNAME", ["lb.example.net."])
    service(["192.0.2.2", "192.0.2.3", "2001:db8::2"], 600)
    time.sleep(9)
    assert values("service", "A") == ["192.0.2.1", "192.0.2.2"], "create-only changed targets"
    print("PASS: create-only preserves existing targets", flush=True)
    release(policy="upsert-only")
    wait_for(lambda: values("service", "A") == ["192.0.2.2", "192.0.2.3"] and values("service", "AAAA") == ["2001:db8::2"], "multi-target replacement")
    assert all(r["ttl"] == 600 for r in records() if r["name"].rstrip(".") == "service." + domain)
    service(["192.0.2.2", "192.0.2.3", "2001:db8::2"], 300)
    wait_for(lambda: all(r["ttl"] == 300 for r in records() if r["name"].rstrip(".") == "service." + domain), "TTL-only update")
    kubectl("delete", "ingress", "test-ingress")
    time.sleep(9)
    assert values("ingress", "CNAME") == ["lb.example.net"], "upsert-only deleted DNS"
    print("PASS: upsert-only retains removed resource DNS", flush=True)
    release(policy="sync")
    wait_for(lambda: not values("ingress", "CNAME") and not values("edns-cname.ingress", "TXT"), "sync deletes data and ownership")
    service(["first.example.net"])
    wait_for(lambda: values("service", "CNAME") == ["first.example.net"] and not values("service", "A") and not values("service", "AAAA"), "A/AAAA to CNAME transition")
    service(["second.example.net"])
    wait_for(lambda: values("service", "CNAME") == ["second.example.net"], "selected CNAME target update")
    kubectl("rollout", "restart", "deployment/external-dns")
    kubectl("rollout", "status", "deployment/external-dns", "--timeout=180s")
    if not args.live:
        time.sleep(6)
        writes = request("GET", "/test/state")["writes"]
        time.sleep(9)
        assert request("GET", "/test/state")["writes"] == writes, "steady state wrote DNS"
    print("PASS: restart and steady-state reconciliation", flush=True)
    kubectl("delete", "service", "test-service")
    wait_for(lambda: len(records()) == len(manual), "sync removes only this controller's records")
    remaining = {r["id"]: r for r in records()}
    assert all(remaining[r["id"]] == r for r in manual), "unrelated or foreign records changed"
    print("PASS: manual, foreign-owner, and ACME records preserved", flush=True)
    if args.live:
        public_dns("service", "A", [])
        public_dns("service", "AAAA", [])
        public_dns("ingress", "CNAME", [])
    succeeded = True
finally:
    if created_cluster:
        cleaned = not args.live
        try:
            if not succeeded:
                for container in ["external-dns", "webhook"]:
                    (work / (container + ".log")).write_text(kubectl("logs", "deployment/external-dns", "-c", container, "--tail=100", check=False))
                print("Test diagnostics retained locally:", work, flush=True)
            kubectl("scale", "deployment/external-dns", "--replicas=0", check=False)
            if args.live:
                kubectl("wait", "--for=delete", "pod", "-l", "app.kubernetes.io/instance=external-dns", "--timeout=90s")
                for record in records():
                    request("DELETE", f"/v1/zones/{zone_id}/records/" + urllib.parse.quote(record["id"], safe=""))
                assert not records(), "live cleanup incomplete"
                cleaned = True
                print("PASS: live test records cleaned up", flush=True)
        finally:
            if forward:
                forward.terminate()
                forward.wait(timeout=10)
            if not args.keep_cluster:
                command("kind", "delete", "cluster", "--name", run_id)
                if succeeded and cleaned:
                    shutil.rmtree(work)
            else:
                print("Fixture diagnostics directory:", work, flush=True)
