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

from app.services.search.follower_search import FollowerSearch


class TestFollowerSearch:
    @pytest.mark.parametrize(
        "for_user, term, offset, limit",
        [
            ("test-user-1", "a-us", 100, 10),
            ("test-user-2", "b", 0, 10),
            ("test-user-3", "test_1", 20, 20),
            ("test-user-3", "test_1", 0, 100),
            ("test-user-4", "test_2", 100, 100),
            ("test-user-5", "test_3", 100, 50),
        ],
    )
    @pytest.mark.asyncio
    async def test_follower_query(
        self,
        for_user: str,
        term: str,
        offset: int,
        limit: int,
    ):
        follower_search: FollowerSearch = FollowerSearch(for_user)
        generated_query: str = follower_search.query(
            term=term, offset=offset, limit=limit
        )
        expected_query = (
            f"SELECT uf.user_id as u_id FROM users_follow uf "
            f"INNER JOIN (SELECT id from users_user "
            f"WHERE username = LOWER('{for_user}')) target_user "
            f"ON target_user.id = uf.target_user_id "
            f"INNER JOIN users_user follower_user "
            f"ON follower_user.id = uf.user_id AND follower_user.username LIKE LOWER('%{term}%') "
            f"LIMIT {limit} OFFSET {offset}"
        )
        assert generated_query == expected_query

    @pytest.mark.parametrize(
        "for_user, term",
        [
            ("test-user-1", "a-us"),
            ("test-user-2", "b"),
            ("test-user-3", "test_1"),
            ("test-user-3", "test_1"),
            ("test-user-4", "test_2"),
            ("test-user-5", "test_3"),
        ],
    )
    @pytest.mark.asyncio
    async def test_follower_count_query(self, term: str, for_user: str):
        for_user = "test-user-01"
        follower_search: FollowerSearch = FollowerSearch(for_user)
        generated_query: str = follower_search.count_query(term=term)

        assert generated_query == (
            f"SELECT count(1) as count FROM users_follow uf "
            f"INNER JOIN (SELECT id from users_user "
            f"WHERE username = LOWER('{for_user}')) target_user "
            f"ON target_user.id = uf.target_user_id "
            f"INNER JOIN users_user follower_user "
            f"ON follower_user.id = uf.user_id AND follower_user.username LIKE LOWER('%{term}%')"
        )
