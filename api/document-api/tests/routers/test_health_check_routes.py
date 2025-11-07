"""
Copyright (c) 2024 Prashant Srivastav <prashant@rapida.ai>
All rights reserved.

This code is licensed under the MIT License. You may obtain a copy of the License at
https://opensource.org/licenses/MIT.

Unless required by applicable law or agreed to in writing, software distributed under the
License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
either express or implied. See the License for the specific language governing permissions
and limitations under the License.

"""
import pytest
from async_asgi_testclient import TestClient as AsyncTestClient


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "expected_response, expected_status",
    [
        ({"content": {"healthy": True}, "code": 200, "success": True}, 200),
    ],
)
async def test_health_check(
    async_api_client: AsyncTestClient, expected_response, expected_status
):
    response = await async_api_client.get("/healthz/")
    assert response.json() == expected_response
    assert response.status_code == expected_status


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "expected_response, expected_status",
    [
        (
            {
                "success": True,
                "content": {},
                "code": 200,
            },
            200,
        ),
    ],
)
async def test_readiness(
    async_api_client: AsyncTestClient, expected_response, expected_status
):
    response = await async_api_client.get("/readiness/")
    assert response.json() == expected_response
    assert response.status_code == expected_status
