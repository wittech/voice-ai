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

from app.services.search.clip_search import ClipSearch


class TestClipSearchQueryGeneration:
    @pytest.mark.parametrize(
        "term",
        ["Naija", "hate", "love-tested", "hate-tested", "lomotif-tested"],
    )
    @pytest.mark.asyncio
    async def test_clip_query(
        self,
        term: str,
        monkeypatch,
    ):
        clip_search: ClipSearch = ClipSearch()
        generated_query: Mapping[str, Any] = clip_search.query(term=term)
        output_query_json: Mapping[str, Any] = {
            "_source": ["id"],
            "query": {
                "bool": {
                    "must": [
                        {"bool": {"should": {"exists": {"field": "name"}}}},
                        {
                            "query_string": {
                                "query": f'name:*"{term}"* OR tags:*"{term}"*'
                            }
                        },
                    ],
                    "must_not": {"match": {"banned": True}},
                    "filter": {"term": {"privacy": 3}},
                }
            },
        }
        assert output_query_json == generated_query
