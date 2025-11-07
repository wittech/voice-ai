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
from typing import Any, Mapping

import pytest

from app.services.search.user_search import UserSearch


class TestUserSearchQueryGeneration:
    @pytest.mark.parametrize(
        "term",
        ["love", "hate", "love-tested", "hate-tested", "lomotif-tested"],
    )
    @pytest.mark.asyncio
    async def test_user_query(
        self,
        term: str,
        monkeypatch,
    ):
        user_search: UserSearch = UserSearch()
        content_type: str = "user"
        generated_query: Mapping[str, Any] = user_search.query(
            term=term, content_type=content_type
        )
        output_query_json: Mapping[str, Any] = {
            "_source": ["id"],
            "query": {
                "bool": {
                    "must": [
                        {"match": {"type": content_type}},
                        {
                            "bool": {
                                "should": [
                                    {
                                        "match": {
                                            "name.keyword": {
                                                "query": term,
                                                "boost": 1,
                                            },
                                        }
                                    },
                                    {
                                        "match": {
                                            "name": {
                                                "query": term,
                                                "fuzziness": "AUTO",
                                                "boost": 0.8,
                                            },
                                        }
                                    },
                                    {
                                        "wildcard": {
                                            "name": {"value": f"{term}*", "boost": 0.7}
                                        }
                                    },
                                    {
                                        "wildcard": {
                                            "name": {"value": f"{term}^4", "boost": 0.6}
                                        }
                                    },
                                ]
                            }
                        },
                    ],
                    "must_not": {"match": {"banned": True}},
                }
            },
        }
        assert output_query_json == generated_query
