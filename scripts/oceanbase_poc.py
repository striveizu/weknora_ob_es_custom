#!/usr/bin/env python3
"""Control only the isolated weknora-ob-poc containers. No production reset.

Credentials are created once with mode 0600. SQL/passwords are never printed.
The local Go builder used by this workspace is configurable via POC_GO_IMAGE.
"""
import argparse
import io
import os
from pathlib import Path
import secrets
import string
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]
IMAGE = "oceanbase/oceanbase-ce:4.4.2.1-101000022026050611"
NETWORK = "weknora-ob-poc"


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def password():
    return "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(28))


def env(name):
    return dict(line.split("=", 1) for line in (ROOT / name).read_text().splitlines() if "=" in line and not line.startswith("#"))


def save(name, values):
    path = ROOT / name
    if not path.exists():
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as output:
            output.write("".join(key + "=" + value + "\n" for key, value in values.items()))


def credentials():
    save(".env.oceanbase-poc", {
        "DB_DRIVER": "mysql", "DB_HOST": "weknora-ob-poc-db", "DB_PORT": "2881",
        "DB_USER": "weknora_poc@test", "DB_PASSWORD": password(), "DB_NAME": "weknora_poc",
        "DB_MYSQL_TLS": "false", "AUTO_MIGRATE": "false",
        "RETRIEVE_DRIVER": "elasticsearch_v8", "ELASTICSEARCH_ADDR": "http://weknora-ob-poc-es:9200",
        "ELASTICSEARCH_INDEX": "weknora_poc_chunks_v1", "REDIS_ADDR": "weknora-ob-poc-redis:6379",
        "REDIS_PASSWORD": password(), "STORAGE_TYPE": "local", "LOCAL_STORAGE_BASE_DIR": "/data/files",
        "JWT_SECRET": password(), "GIN_MODE": "release", "LOG_LEVEL": "info",
        "NEO4J_ENABLE": "false", "DOCREADER_ADDR": "weknora-ob-poc-docreader:50051",
    })
    save(".env.oceanbase-poc-admin", {"OB_ADMIN_PASSWORD": password(), "OB_TEST_PASSWORD": password()})
    app, admin = env(".env.oceanbase-poc"), env(".env.oceanbase-poc-admin")
    save(".env.oceanbase-poc-tests", {
        "WEKNORA_OCEANBASE_TEST_DSN": "weknora_test@test:" + admin["OB_TEST_PASSWORD"] + "@tcp(weknora-ob-poc-db:2881)/?parseTime=true&loc=UTC&timeout=10s&readTimeout=300s",
        "REDIS_PASSWORD": app["REDIS_PASSWORD"],
    })
    print("PoC credentials: .env.oceanbase-poc (0600, ignored by Git)")


def exists(name):
    return subprocess.run(["docker", "container", "inspect", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def start(name, args):
    if exists(name):
        run(["docker", "start", name], stdout=subprocess.DEVNULL)
    else:
        run(["docker", "run", "-d", "--name", name, "--network", NETWORK] + args, stdout=subprocess.DEVNULL)


def infra():
    credentials()
    app, admin = env(".env.oceanbase-poc"), env(".env.oceanbase-poc-admin")
    if subprocess.run(["docker", "network", "inspect", NETWORK], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        run(["docker", "network", "create", NETWORK], stdout=subprocess.DEVNULL)
    start("weknora-ob-poc-db", ["--memory=7g", "-p", "127.0.0.1:12881:2881",
          "-v", "weknora-ob-poc-data:/root/ob", "-v", "weknora-ob-poc-config:/root/.obd",
          "-e", "MODE=mini", "-e", "OB_MEMORY_LIMIT=6G", "-e", "OB_SYSTEM_MEMORY=1G",
          "-e", "OB_TENANT_MEMORY_SIZE=3G", "-e", "OB_TENANT_MIN_CPU=1",
          "-e", "OB_TENANT_LOG_DISK_SIZE=2G",
          "-e", "OB_DATABASE=weknora_poc", "-e", "OB_TENANT_PASSWORD=" + admin["OB_ADMIN_PASSWORD"],
          "--entrypoint", "bash", IMAGE, "-c",
          "sed -i 's/cpu_count: 16/cpu_count: 8/' /root/templates/observer-template.yaml; /usr/sbin/sshd && /root/boot/start.sh"])
    start("weknora-ob-poc-es", ["--memory=1g", "-p", "127.0.0.1:18920:9200",
          "-v", "weknora-ob-poc-es-data:/usr/share/elasticsearch/data",
          "-e", "discovery.type=single-node", "-e", "xpack.security.enabled=false",
          "-e", "ES_JAVA_OPTS=-Xms512m -Xmx512m", "docker.elastic.co/elasticsearch/elasticsearch:8.19.6"])
    start("weknora-ob-poc-redis", ["--memory=128m", "-v", "weknora-ob-poc-redis-data:/data",
          "redis:7-alpine", "redis-server", "--appendonly", "yes", "--requirepass", app["REDIS_PASSWORD"]])
    print("Infra started; wait for OceanBase tenant initialization, then run accounts.")


def accounts():
    credentials()
    app, admin = env(".env.oceanbase-poc"), env(".env.oceanbase-poc-admin")
    # Random credentials contain only ASCII letters/digits. Reject edited
    # values rather than interpolating arbitrary strings into DDL.
    for value in [app["DB_PASSWORD"], admin["OB_TEST_PASSWORD"], admin["OB_ADMIN_PASSWORD"]]:
        if not value.isascii() or not value.isalnum():
            raise RuntimeError("This local bootstrap requires generated alphanumeric credentials")
    sql = (
        "CREATE DATABASE IF NOT EXISTS weknora_poc DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;\n"
        "CREATE USER IF NOT EXISTS weknora_poc IDENTIFIED BY '" + app["DB_PASSWORD"] + "';\n"
        "GRANT ALL ON weknora_poc.* TO weknora_poc;\n"
        "CREATE USER IF NOT EXISTS weknora_test IDENTIFIED BY '" + admin["OB_TEST_PASSWORD"] + "';\n"
        "GRANT ALL ON `weknora\\_ob\\_test\\_%`.* TO weknora_test;\n"
    )
    base = ["docker", "exec", "-i", "-e", "MYSQL_PWD=" + admin["OB_ADMIN_PASSWORD"],
            "weknora-ob-poc-db", "obclient", "-h127.0.0.1", "-P2881", "-uroot@test"]
    # Capture diagnostics because SQL clients can echo statements containing
    # passwords on failures. Return a redacted error instead.
    result = subprocess.run(base, input=sql, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError("PoC account bootstrap failed; verify the tenant is ready and the local admin credential matches")
    print("Application database/user and restricted integration-test user ready.")


def builder():
    name = "weknora-ob-poc-build"
    if not exists(name):
        run(["docker", "run", "-d", "--name", name, "--network", NETWORK,
             "-v", str(ROOT) + ":/workspace", "-w", "/workspace", "--entrypoint", "sleep",
             os.getenv("POC_GO_IMAGE", "weknora-app-builder:auto-register"), "infinity"], stdout=subprocess.DEVNULL)
    else:
        run(["docker", "start", name], stdout=subprocess.DEVNULL)
    return name


def migrate():
    run(["docker", "exec", "--env-file", str(ROOT / ".env.oceanbase-poc"), builder(),
         "go", "run", "./cmd/oceanbase-migrate"])


def build():
    run(["docker", "exec", builder(), "go", "build", "-p", "2", "-o", "/workspace/build/WeKnora-ob-poc", "./cmd/server"])
    # gojieba locates its dictionaries relative to the compiled module path.
    # Match the current go.mod version, not the older cached builder image.
    dict_dir=ROOT / "build/jieba-dict"
    dict_dir.mkdir(parents=True,exist_ok=True)
    # Go module cache files are read-only; make the local artifact writable
    # before copying the same dictionary version again.
    for path in dict_dir.rglob("*"):
        path.chmod(0o755 if path.is_dir() else 0o644)
    result=run(["docker","exec",builder(),"tar","-C","/go/pkg/mod/github.com/yanyiwu/gojieba@v1.4.7/deps/cppjieba/dict","-czf","-","."],capture_output=True)
    with tarfile.open(fileobj=io.BytesIO(result.stdout),mode="r:gz") as archive:
        members=archive.getmembers()
        for member in members:
            if not (member.isfile() or member.isdir()) or not (dict_dir/member.name).resolve().is_relative_to(dict_dir.resolve()):
                raise RuntimeError("Unexpected dictionary archive entry")
            member.mode=0o755 if member.isdir() else 0o644
        archive.extractall(dict_dir,members=members)


def test():
    run(["docker", "exec", "--env-file", str(ROOT / ".env.oceanbase-poc-tests"), builder(),
         "go", "test", "./internal/application/repository", "-run", "TestOceanBaseWikiIntegration", "-count=1", "-v"])


def app():
    if not (ROOT / "build/WeKnora-ob-poc").exists() or not (ROOT/"build/jieba-dict/jieba.dict.utf8").exists():
        build()
    start("weknora-ob-poc-docreader", ["--memory=768m", "wechatopenai/weknora-docreader:latest"])
    start("weknora-ob-poc-app", ["--memory=1g", "--network-alias", "app", "-p", "127.0.0.1:18080:8080",
          "--env-file", str(ROOT / ".env.oceanbase-poc"), "-v", str(ROOT) + ":/workspace:ro",
          "-v", str(ROOT/"build/jieba-dict")+":/go/pkg/mod/github.com/yanyiwu/gojieba@v1.4.7/deps/cppjieba/dict:ro",
          "-v", "weknora-ob-poc-files:/data/files", "-w", "/workspace",
          "--entrypoint", "/workspace/build/WeKnora-ob-poc", os.getenv("POC_GO_IMAGE", "weknora-app-builder:auto-register")])
    start("weknora-ob-poc-ui", ["--memory=128m", "-p", "127.0.0.1:18081:80", "wechatopenai/weknora-ui:latest"])
    print("PoC UI: http://127.0.0.1:18081 ; backend: http://127.0.0.1:18080")


def stop():
    for suffix in ["ui", "app", "docreader", "redis", "es", "db", "build"]:
        name = "weknora-ob-poc-" + suffix
        if exists(name):
            run(["docker", "stop", name], stdout=subprocess.DEVNULL)
    print("Only PoC containers stopped; volumes and credentials preserved.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["credentials", "infra", "accounts", "migrate", "build", "test", "app", "stop"])
    args = parser.parse_args()
    try:
        globals()[args.command]()
    except (subprocess.CalledProcessError, RuntimeError) as error:
        # Do not print CalledProcessError.cmd: docker arguments may include
        # generated passwords.
        parser.exit(1, "PoC command failed: " + (str(error) if isinstance(error, RuntimeError) else "subprocess failed") + "\n")
