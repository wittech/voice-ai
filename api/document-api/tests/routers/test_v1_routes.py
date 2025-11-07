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
import json
from typing import List, Optional, Tuple
from unittest.mock import MagicMock

import pytest
from async_asgi_testclient import TestClient as AsyncTestClient
from bridges import ClipBridge, FeedBridge, MusicBridge, UserBridge
from pydantic import HttpUrl

# from bridges import clip_bridge, feed_bridge, music_bridge, user_bridge
from app.bridges.bridge_factory import (
    get_me_clip_service_client,
    get_me_feed_service_client,
    get_me_music_service_client,
    get_me_user_service_client,
)
from app.configs.auth_config import TokenConfig
from app.connectors.connector_factory import (
    get_me_elastic_search,
    get_me_music_elastic_search,
    get_me_postgres,
    get_me_redis,
)
from app.exceptions.authentication_exception import InvalidAuthorizationTokenException
from app.main import app
from app.middlewares import TokenAuthorizationMiddleware
from app.services.search import (
    channel_search,
    clip_search,
    follower_search,
    following_search,
    hashtag_search,
    music_search,
    top_search,
    user_channel_search,
    user_search,
)


def user_service_test_client() -> UserBridge:
    return UserBridge(host=HttpUrl(url="localhost:8080", scheme="https"))


def clip_service_test_client() -> ClipBridge:
    return ClipBridge(host=HttpUrl(url="localhost:8080", scheme="https"))


def feed_service_test_client() -> FeedBridge:
    return FeedBridge(host=HttpUrl(url="localhost:8080", scheme="https"))


def music_service_test_client() -> MusicBridge:
    return MusicBridge(host=HttpUrl(url="localhost:8080", scheme="https"))


get_me_test_client = MagicMock()
get_me_test_client.return_value = MagicMock()

app.dependency_overrides = {
    get_me_user_service_client: user_service_test_client,
    get_me_clip_service_client: clip_service_test_client,
    get_me_music_service_client: music_service_test_client,
    get_me_feed_service_client: feed_service_test_client,
    get_me_elastic_search: lambda: MagicMock(),
    get_me_redis: lambda: MagicMock(),
    get_me_music_elastic_search: lambda: MagicMock(),
    get_me_postgres: lambda: MagicMock(),
}


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "expected_response, expected_status",
    [
        (
            {"content": {"ping": "pong"}, "success": True, "code": 200},
            200,
        ),
    ],
)
async def test_v1_ping(
    async_api_client: AsyncTestClient,
    expected_response,
    expected_status,
):
    response = await async_api_client.get("/v1/ping/")
    assert response.json() == expected_response
    assert response.status_code == expected_status


class TestV1UserSearch:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/user-elastic-search-response.json",
            ) as file:
                user_search_response = file.read()
            el_result = json.loads(user_search_response)
            return (
                len(el_result),
                el_result,
            )

        async def mock_user_profile_by_ids(*args, ids: List[int]) -> List:
            length, data = await mock_searching()
            assert len(ids) == length
            assert [int(dct["id"]) for dct in data] == ids
            with open(
                "tests/routers/example_data/profile-user-service-response.json",
            ) as file:
                profile_response = file.read()
            return json.loads(profile_response)

        monkeypatch.setattr(
            UserBridge,
            "post_for_user_profile_by_ids",
            mock_user_profile_by_ids,
        )
        # mocking service client
        monkeypatch.setattr(user_search.UserSearch, "searching", mock_searching)

    # negative scenario
    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, expected_error_message, expected_status",
        [
            (
                "",
                "validation error for request ensure you have provided all required fields.",
                400,
            )
        ],
    )
    async def test_negative_scenario_with_term_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        expected_error_message,
        expected_status,
    ):
        limit = 10
        offset = 2
        response = await async_api_client.get(
            f"/v1/search/top_search/user/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert False.__eq__(j_response["success"])
        assert j_response["code"] == expected_status
        assert j_response["content"]["error_message"] == expected_error_message

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "limit, expected_error_message, expected_status",
        [
            (
                0,
                "validation error for request ensure you have provided all required fields.",
                400,
            ),
            (
                -2,
                "validation error for request ensure you have provided all required fields.",
                400,
            ),
            (
                -1,
                "validation error for request ensure you have provided all required fields.",
                400,
            ),
            (
                21,
                "validation error for request ensure you have provided all required fields.",
                400,
            ),
        ],
    )
    async def test_negative_scenario_with_limit_param(
        self,
        async_api_client: AsyncTestClient,
        limit: str,
        expected_error_message,
        expected_status,
    ):
        term = "abc"
        offset = 2
        response = await async_api_client.get(
            f"/v1/search/top_search/user/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert False.__eq__(j_response["success"])
        assert j_response["code"] == expected_status
        assert j_response["content"]["error_message"] == expected_error_message

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/top_search/user/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/user-search-result-output.json",
        ) as file:
            user_search_response = file.read()
        assert json.loads(user_search_response) == j_response["results"]


# #
class TestV1ClipSearch:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/clip-elastic-search-response.json",
            ) as file:
                clip_ids_es = file.read()
            el_result = json.loads(clip_ids_es)
            return (
                len(el_result),
                el_result,
            )

        # mocking clip_search.searching call
        monkeypatch.setattr(clip_search.ClipSearch, "searching", mock_searching)

        async def mock_clip_detail_by_id(*args, clip_ids: List[str]) -> List:
            length, data = await mock_searching()
            assert len(clip_ids) == length
            assert data == clip_ids

            with open(
                "tests/routers/example_data/clip-detail-clip-service-response.json",
            ) as file:
                clip_detail = file.read()
            return json.loads(clip_detail)

        monkeypatch.setattr(
            ClipBridge,
            "post_for_clip_detail_by_ids",
            mock_clip_detail_by_id,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/top_search/clip/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/clip-search-result-output.json",
        ) as file:
            clip_search_response = file.read()
        assert json.loads(clip_search_response) == j_response["results"]


class TestV1MusicSearch:
    # country_code
    country_code = "us"

    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/music-elastic-search-response.json",
            ) as file:
                music_ids_es = file.read()
            el_result = json.loads(music_ids_es)
            return (
                len(el_result),
                el_result,
            )

        # mocking clip_search.searching call
        monkeypatch.setattr(music_search.MusicSearch, "searching", mock_searching)

        async def mock_music_detail_by_id(*args, ids: List[str]) -> List:
            length, data = await mock_searching()
            assert len(ids) == length
            assert data == ids

            with open(
                "tests/routers/example_data/music-detail-music-service-response.json",
            ) as file:
                music_detail = file.read()
            return json.loads(music_detail)

        monkeypatch.setattr(
            MusicBridge,
            "post_for_music_detail_by_ids",
            mock_music_detail_by_id,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        async_api_client.headers = {
            "HTTP_CF_IPCOUNTRY": self.country_code,
        }
        response = await async_api_client.get(
            f"/v1/search/top_search/music/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/music-search-result-output.json",
        ) as file:
            music_search_response = file.read()

        assert json.loads(music_search_response) == j_response["results"]

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
        ],
    )
    async def test_negative_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/top_search/music/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] == 0


class TestV1HashtagSearch:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/hashtag-elastic-search-response.json",
            ) as file:
                hashtag = file.read()
            el_result = json.loads(hashtag)
            return (
                len(el_result),
                el_result,
            )

        # mocking clip_search.searching call
        monkeypatch.setattr(hashtag_search.HashtagSearch, "searching", mock_searching)

        async def mock_hashtag_detail_by_ids(*args, hashtag_ids: List) -> List:
            length, data = await mock_searching()
            assert length == len(hashtag_ids)
            assert [objects["id"] for objects in data] == hashtag_ids
            with open(
                "tests/routers/example_data/hashtag-detail-feed-service-response.json",
            ) as file:
                hashtag_detail = file.read()
            return json.loads(hashtag_detail)

        monkeypatch.setattr(
            FeedBridge,
            "post_for_hashtag_detail_by_ids",
            mock_hashtag_detail_by_ids,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/top_search/hashtag/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/hashtag-search-result-output.json",
        ) as file:
            hashtag_search_response = file.read()
        assert json.loads(hashtag_search_response) == j_response["results"]


class TestV1ChannelSearch:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/channel-elastic-search-response.json",
            ) as file:
                channels = file.read()
                el_result = json.loads(channels)
            return (
                len(el_result),
                el_result,
            )

        # mocking searching call
        monkeypatch.setattr(channel_search.ChannelSearch, "searching", mock_searching)

        async def mock_channel_detail_by_ids(*args, channel_ids: List[int]) -> List:
            #  validating channel Ids
            length, data = await mock_searching()
            assert length == len(channel_ids)
            assert [objects["id"] for objects in data] == channel_ids
            with open(
                "tests/routers/example_data/channel-detail-feed-service-response.json",
            ) as file:
                channel_details = file.read()
            return json.loads(channel_details)

        monkeypatch.setattr(
            FeedBridge,
            "post_for_channel_detail_by_ids",
            mock_channel_detail_by_ids,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/top_search/channel/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/channel-search-result-output.json",
        ) as file:
            channel_search_response = file.read()
        assert json.loads(channel_search_response) == j_response["results"]


class TestV1TopSearch:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        def mock_elastic_search_user() -> List:
            with open(
                "tests/routers/example_data/user-elastic-search-response.json",
            ) as file:
                users = file.read()
                return json.loads(users)

        def mock_elastic_search_hashtag() -> List:
            with open(
                "tests/routers/example_data/hashtag-elastic-search-response.json",
            ) as file:
                hashtags = file.read()
                return json.loads(hashtags)

        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            el_result: List = [hashtag for hashtag in mock_elastic_search_hashtag()] + [
                user for user in mock_elastic_search_user()
            ]
            return (
                len(el_result),
                el_result,
            )

        # mocking searching call
        monkeypatch.setattr(top_search.TopSearch, "searching", mock_searching)

        async def mock_user_profile_by_ids(*args, ids: List[int]) -> List:
            data = mock_elastic_search_user()
            assert len(data) == len(ids)
            assert [int(objects["id"]) for objects in data] == ids
            with open(
                "tests/routers/example_data/profile-user-service-response.json",
            ) as file:
                profile_response = file.read()
            return json.loads(profile_response)

        async def mock_hashtag_detail_by_ids(*args, hashtag_ids: List[str]) -> List:
            data = mock_elastic_search_hashtag()
            assert len(data) == len(hashtag_ids)
            assert [objects["id"] for objects in data] == hashtag_ids
            with open(
                "tests/routers/example_data/hashtag-detail-feed-service-response.json",
            ) as file:
                hashtag_detail = file.read()
            return json.loads(hashtag_detail)

        monkeypatch.setattr(
            FeedBridge,
            "post_for_hashtag_detail_by_ids",
            mock_hashtag_detail_by_ids,
        )

        monkeypatch.setattr(
            UserBridge,
            "post_for_user_profile_by_ids",
            mock_user_profile_by_ids,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/top_search/top/?term={term}&limit={limit}&offset={offset}"
        )
        j_response = response.json()
        assert response.status_code == expected_status
        assert j_response["count"] > 0

        complete_response: List = []
        with open(
            "tests/routers/expected_output/hashtag-search-result-output.json",
        ) as file:
            hashtag_response = file.read()
            complete_response = complete_response + [
                htag for htag in json.loads(hashtag_response)
            ]

        with open(
            "tests/routers/expected_output/user-search-result-output.json",
        ) as file:
            user_response = file.read()
            complete_response = complete_response + [
                user for user in json.loads(user_response)
            ]
        assert len(complete_response) == len(j_response["results"])


class TestV1SearchMusicForCreateLomotifFeature:
    country_code = "us"

    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/music-elastic-search-response.json",
            ) as file:
                music_ids_es = file.read()
            el_result = json.loads(music_ids_es)
            return (
                len(el_result),
                el_result,
            )

        # mocking clip_search.searching call
        monkeypatch.setattr(music_search.MusicSearch, "searching", mock_searching)

        async def mock_music_detail_by_id(*args, ids: List[str]) -> List:
            length, data = await mock_searching()
            assert len(ids) == length
            assert data == ids

            with open(
                "tests/routers/example_data/music-detail-music-service-response.json",
            ) as file:
                music_detail = file.read()
            return json.loads(music_detail)

        monkeypatch.setattr(
            MusicBridge,
            "post_for_music_detail_by_ids",
            mock_music_detail_by_id,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test1", 0, 10, 200),
            ("test2", 1, 1, 200),
            ("test1", 4, 2, 200),
            ("test1", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        async_api_client.headers = {
            "HTTP_CF_IPCOUNTRY": self.country_code,
        }
        response = await async_api_client.get(
            f"/v1/search/music/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/music-search-for-create-lomotif-result-output.json",
        ) as file:
            music_search_response = file.read()
        assert json.loads(music_search_response) == j_response["results"]

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [("test1", 0, 10, 200)],
    )
    async def test_negative_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/music/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] == 0


class TestV1SearchFollowerUser:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/follower-search-user-ids.json",
            ) as file:
                user_ids = file.read()
            db_results: List = json.loads(user_ids)
            return (
                len(db_results),
                db_results,
            )

        monkeypatch.setattr(follower_search.FollowerSearch, "searching", mock_searching)

        async def mock_user_profile_by_ids(*args, ids: List[int]) -> List:
            length, data = await mock_searching()
            assert len(ids) == length
            assert [int(dct) for dct in data] == ids
            with open(
                "tests/routers/example_data/follower-user-profile-user-service-response.json",
            ) as file:
                profile_response = file.read()
            return json.loads(profile_response)

        monkeypatch.setattr(
            UserBridge,
            "post_for_user_profile_by_ids",
            mock_user_profile_by_ids,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "target_user, searched_user, offset, limit, expected_status",
        [
            ("t_user_1", "s_user_1", 0, 10, 200),
            ("t_user_2", "s_user_2", 1, 1, 200),
            ("t_user_3", "s_user_3", 4, 2, 200),
            ("t_user_4", "s_user_4", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        target_user: str,
        searched_user: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/user/{target_user}/{searched_user}/followers/?limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/user-followers-search-result-output.json",
        ) as file:
            music_search_response = file.read()
        assert json.loads(music_search_response) == j_response["results"]


class TestV1SearchFollowingUser:
    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/following-search-user-ids.json",
            ) as file:
                user_ids = file.read()
            db_results: List = json.loads(user_ids)
            return (
                len(db_results),
                db_results,
            )

        monkeypatch.setattr(
            following_search.FollowingSearch, "searching", mock_searching
        )

        async def mock_user_profile_by_ids(*args, ids: List[int]) -> List:
            length, data = await mock_searching()
            assert len(ids) == length
            assert [int(dct) for dct in data] == ids
            with open(
                "tests/routers/example_data/following-user-profile-user-service-response.json",
            ) as file:
                profile_response = file.read()
            return json.loads(profile_response)

        monkeypatch.setattr(
            UserBridge,
            "post_for_user_profile_by_ids",
            mock_user_profile_by_ids,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "target_user, searched_user, offset, limit, expected_status",
        [
            ("t_user_1", "s_user_1", 0, 10, 200),
            ("t_user_2", "s_user_2", 1, 1, 200),
            ("t_user_3", "s_user_3", 4, 2, 200),
            ("t_user_4", "s_user_4", 2, 5, 200),
        ],
    )
    async def test_positive_scenario_with_valid_param(
        self,
        async_api_client: AsyncTestClient,
        target_user: str,
        searched_user: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/user/{target_user}/{searched_user}/following/?limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/user-following-search-result-output.json",
        ) as file:
            music_search_response = file.read()
        assert json.loads(music_search_response) == j_response["results"]


class TestChannelSearchForUser:
    config: TokenConfig = TokenConfig(strict=False, enable=True)

    @pytest.fixture(autouse=True)
    def setup_method(self, monkeypatch):
        async def mock_searching(*args, **kwargs) -> Tuple[int, Optional[List]]:
            with open(
                "tests/routers/example_data/user-channel-search.json",
            ) as file:
                user_ids = file.read()
            db_results: List = json.loads(user_ids)
            return (
                len(db_results),
                db_results,
            )

        async def mock_user_info(token: str):
            assert token is not None
            if token == "valid-token":
                return {"user_id": 1, "is_staff": True, "email": ""}
            else:
                return {}

        app.add_middleware(
            TokenAuthorizationMiddleware,
            config=self.config,
            user_info_resolver=mock_user_info,
        )
        monkeypatch.setattr(
            user_channel_search.UserChannelSearch, "searching", mock_searching
        )

        async def mock_post_for_channel_detail_by_ids(*args, channel_ids: List) -> List:
            length, data = await mock_searching()
            assert len(channel_ids) == length
            with open(
                "tests/routers/example_data/channel-feed-service-response.json",
            ) as file:
                channel_response = file.read()
            return json.loads(channel_response)

        monkeypatch.setattr(
            FeedBridge,
            "post_for_channel_detail_by_ids",
            mock_post_for_channel_detail_by_ids,
        )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test", 0, 10, 200),
            ("test_1", 1, 1, 200),
            ("no_text", 4, 2, 200),
        ],
    )
    async def test_unauthenticated_request(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/channel/user/?term={term}&limit={limit}&offset={offset}"
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] == 0

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test", 0, 10, 200),
            ("test_1", 1, 1, 200),
            ("no_text", 4, 2, 200),
        ],
    )
    async def test_invalid_token_request(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        with pytest.raises(InvalidAuthorizationTokenException):
            await async_api_client.get(
                f"/v1/search/channel/user/?term={term}&limit={limit}&offset={offset}",
                headers={
                    "Authorization": "illegal-token",
                },
            )

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        "term, offset, limit, expected_status",
        [
            ("test", 0, 10, 200),
            ("test_1", 1, 1, 200),
            ("no_text", 4, 2, 200),
        ],
    )
    async def test_valid_request(
        self,
        async_api_client: AsyncTestClient,
        term: str,
        offset: int,
        limit: int,
        expected_status: int,
    ):
        response = await async_api_client.get(
            f"/v1/search/channel/user/?term={term}&limit={limit}&offset={offset}",
            headers={
                "Authorization": "Token valid-token",
            },
        )
        assert response.status_code == expected_status
        j_response = response.json()
        assert j_response["count"] > 0
        with open(
            "tests/routers/expected_output/channel-search-for-user-search-result-output.json",
        ) as file:
            music_search_response = file.read()
        assert json.loads(music_search_response) == j_response["results"]
