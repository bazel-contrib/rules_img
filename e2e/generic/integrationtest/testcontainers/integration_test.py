import json
import docker
import requests
import os
from testcontainers.core.container import DockerContainer
from testcontainers.core.wait_strategies import LogMessageWaitStrategy
from python.runfiles import Runfiles

r = Runfiles.Create()
TAR_PATH = r.Rlocation("_main/integrationtest/testcontainers/neo4j_for_host_docker_oci_layout.tar")

def _load_latest_tarball():
    client = docker.from_env()
    with open(TAR_PATH, "rb") as f:
        images = client.images.load(f)
    if len(images) != 1:
        raise RuntimeError(f"Expected exactly one image to be loaded, got {len(images)}")
    return images[0]

def test_container_runs():
    image = _load_latest_tarball()

    user = os.environ["USER"]

    with DockerContainer(
        image.id,
    ).with_bind_ports(
        container=7474,
    ).with_bind_ports(
        container=7687,
    ).waiting_for(LogMessageWaitStrategy("Started.")) as container:
        http_port = container.get_exposed_port(7474)

        # Test HTTP port is responding
        response = requests.get(f"http://localhost:{http_port}")
        assert response.status_code == 200, f"Expected HTTP 200, got {response.status_code}"

test_container_runs()
