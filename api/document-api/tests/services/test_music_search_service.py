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

#  instantiate music search object
from typing import List

import pytest

from app.configs.music_filter_config import MusicFilterConfig
from app.connectors.redis_connector import RedisConnector
from app.services.search.music_search import MusicSearch


def get_testable_music_search(
    redis_test_connector: RedisConnector,
    filter_config: MusicFilterConfig,
    country_code: str,
) -> MusicSearch:
    return MusicSearch(
        redis_client=redis_test_connector,
        filter_config=filter_config,
        country_code=country_code,
    )


class TestMusicSearchService:
    @pytest.mark.parametrize(
        "key, generated_redis_key, country_code",
        [
            (
                "some-white-listed-artists.txt",
                "music_filter:_somewhitelistedartiststxt",
                "us",
            ),
            ("some-white-listed-artists", "music_filter:_somewhitelistedartists", "in"),
            ("some-black-listed-artists", "music_filter:_someblacklistedartists", "IN"),
            (
                "some-black-listed-artists.txt.cst",
                "music_filter:_someblacklistedartiststxtcst",
                "ts",
            ),
            (".txt.cst", "music_filter:_txtcst", "GB"),
        ],
    )
    @pytest.mark.asyncio
    async def test_generate_filtered_artists_redis_key(
        self,
        redis_test_connector: RedisConnector,
        key: str,
        generated_redis_key: str,
        country_code: str,
        monkeypatch,
    ):
        filter_config: MusicFilterConfig = MusicFilterConfig()
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )
        assert generated_redis_key == _ms.generate_filtered_artists_redis_key(key)

    @pytest.mark.parametrize(
        "whitelisted_source, term, country_code",
        [
            (["source_a", "source_b", "source_c"], "char", "us"),
            (["source_a"], "single_char", "US"),
            (["source_1", "source_2", "source_3"], "char", "IN"),
            (["source_6", "source_5", "source_4"], "op_num", "in"),
        ],
    )
    @pytest.mark.asyncio
    async def test_query_for_music_search_with_whitelisting_source(
        self,
        redis_test_connector: RedisConnector,
        whitelisted_source: List,
        term: str,
        country_code: str,
        monkeypatch,
    ):
        filter_config: MusicFilterConfig = MusicFilterConfig(
            whitelisted_source=whitelisted_source
        )
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )
        generated_query = await _ms.query(term=term)

        output_query_json = {
            "_source": "false",
            "query": {
                "bool": {
                    "must": [
                        {
                            "match": {
                                "music_search_field": {
                                    "query": term,
                                    "fuzziness": "AUTO",
                                }
                            }
                        },
                        {
                            "bool": {
                                "should": [
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "terms": {
                                                        "source": whitelisted_source
                                                    }
                                                }
                                            ]
                                        }
                                    }
                                ]
                            }
                        },
                    ],
                    "filter": {
                        "bool": {
                            "must": [
                                {"term": {"status": "active"}},
                                {
                                    "bool": {
                                        "should": [
                                            {
                                                "bool": {
                                                    "must_not": [
                                                        {"term": {"source": "7digital"}}
                                                    ]
                                                }
                                            },
                                            {
                                                "bool": {
                                                    "must": [
                                                        {
                                                            "term": {
                                                                "source": "7digital"
                                                            }
                                                        },
                                                        {
                                                            "term": {
                                                                "country_code": country_code.lower()
                                                            }
                                                        },
                                                    ]
                                                }
                                            },
                                        ]
                                    }
                                },
                            ]
                        }
                    },
                }
            },
        }
        assert output_query_json == generated_query

    @pytest.mark.parametrize(
        "whitelisted_source, term, whitelisted_artist_s3_key",
        [
            (
                ["source_a", "source_b", "source_c"],
                "char",
                "some-white-listed-artists.txt",
            ),
            (["source_a"], "single_char", "some-white-listed-artists.tus.tm"),
            (
                ["source_1", "source_2", "source_3"],
                "char",
                "some-white-listed-artists-01.csv",
            ),
            (
                ["source_6", "source_5", "source_4"],
                "op_num",
                "some-white-listed-artists",
            ),
        ],
    )
    @pytest.mark.asyncio
    async def test_query_for_music_search_with_whitelisting_source_and_whitelisted_artist_s3_key(
        self,
        redis_test_connector: RedisConnector,
        whitelisted_source: List,
        term: str,
        whitelisted_artist_s3_key: str,
        monkeypatch,
    ):
        _whitelisted_fuzz_score = 1
        country_code: str = "us"
        filter_config: MusicFilterConfig = MusicFilterConfig(
            whitelisted_source=whitelisted_source,
            whitelisted_artist_s3_key=whitelisted_artist_s3_key,
            whitelisted_fuzz_score=_whitelisted_fuzz_score,
            auth={"region": "us-east-1"},
        )
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )

        async def redis_cached_artists(*args, **kwargs):
            assert kwargs == {
                "name": _ms.generate_filtered_artists_redis_key(
                    whitelisted_artist_s3_key
                )
            }
            return ["some-artist-1", "some-artist-2"]

        monkeypatch.setattr(
            redis_test_connector,
            "operate",
            redis_cached_artists,
        )
        generated_query = await _ms.query(term=term)

        output_query_json = {
            "_source": "false",
            "query": {
                "bool": {
                    "must": [
                        {
                            "match": {
                                "music_search_field": {
                                    "query": term,
                                    "fuzziness": "AUTO",
                                }
                            }
                        },
                        {
                            "bool": {
                                "should": [
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "terms": {
                                                        "source": whitelisted_source
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-artist-1",
                                                            "fuzziness": _whitelisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-artist-2",
                                                            "fuzziness": _whitelisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                ]
                            }
                        },
                    ],
                    "filter": {
                        "bool": {
                            "must": [
                                {"term": {"status": "active"}},
                                {
                                    "bool": {
                                        "should": [
                                            {
                                                "bool": {
                                                    "must_not": [
                                                        {"term": {"source": "7digital"}}
                                                    ]
                                                }
                                            },
                                            {
                                                "bool": {
                                                    "must": [
                                                        {
                                                            "term": {
                                                                "source": "7digital"
                                                            }
                                                        },
                                                        {
                                                            "term": {
                                                                "country_code": country_code.lower()
                                                            }
                                                        },
                                                    ]
                                                }
                                            },
                                        ]
                                    }
                                },
                            ]
                        }
                    },
                }
            },
        }
        # print(generated_query)
        assert output_query_json == generated_query

    @pytest.mark.parametrize(
        "whitelisted_source, term, whitelisted_artist_s3_key",
        [
            (
                ["source_a", "source_b", "source_c", "7digital"],
                "char",
                "some-white-listed-artists.txt",
            ),
            (
                ["source_a", "7digital"],
                "single_char",
                "some-white-listed-artists.tus.tm",
            ),
            (
                ["source_1", "source_2", "source_3", "7digital"],
                "char",
                "some-white-listed-artists-01.csv",
            ),
            (
                ["source_6", "source_5", "source_4", "7digital"],
                "op_num",
                "some-white-listed-artists",
            ),
        ],
    )
    @pytest.mark.asyncio
    async def test_query_for_music_search_with_whitelisting_source_with_7digital_and_whitelisted_artist_s3_key(
        self,
        redis_test_connector: RedisConnector,
        whitelisted_source: List,
        term: str,
        whitelisted_artist_s3_key: str,
        monkeypatch,
    ):
        _whitelisted_fuzz_score = 1
        country_code: str = "us"
        filter_config: MusicFilterConfig = MusicFilterConfig(
            whitelisted_source=whitelisted_source,
            whitelisted_artist_s3_key=whitelisted_artist_s3_key,
            whitelisted_fuzz_score=_whitelisted_fuzz_score,
            auth={"region": "us-east-1"},
        )
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )

        async def redis_cached_artists(*args, **kwargs):
            assert kwargs == {
                "name": _ms.generate_filtered_artists_redis_key(
                    whitelisted_artist_s3_key
                )
            }
            return ["some-artist-1", "some-artist-2"]

        monkeypatch.setattr(
            redis_test_connector,
            "operate",
            redis_cached_artists,
        )
        generated_query = await _ms.query(term=term)

        output_query_json = {
            "_source": "false",
            "query": {
                "bool": {
                    "must": [
                        {
                            "match": {
                                "music_search_field": {
                                    "query": term,
                                    "fuzziness": "AUTO",
                                }
                            }
                        },
                        {
                            "bool": {
                                "should": [
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "terms": {
                                                        "source": whitelisted_source
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-artist-1",
                                                            "fuzziness": _whitelisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-artist-2",
                                                            "fuzziness": _whitelisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                ]
                            }
                        },
                    ],
                    "filter": {
                        "bool": {
                            "must": [
                                {"term": {"status": "active"}},
                                {
                                    "bool": {
                                        "should": [
                                            {
                                                "bool": {
                                                    "must_not": [
                                                        {"term": {"source": "7digital"}}
                                                    ]
                                                }
                                            },
                                            {
                                                "bool": {
                                                    "must": [
                                                        {
                                                            "term": {
                                                                "source": "7digital"
                                                            }
                                                        },
                                                        {
                                                            "term": {
                                                                "country_code": country_code.lower()
                                                            }
                                                        },
                                                    ]
                                                }
                                            },
                                        ]
                                    }
                                },
                            ]
                        }
                    },
                }
            },
        }
        assert output_query_json == generated_query

    @pytest.mark.parametrize(
        "whitelisted_source, term, whitelisted_artist_s3_key, blacklisted_artist_s3_key",
        [
            (
                ["source_a", "source_b", "source_c"],
                "char",
                "some-white-listed-artists.txt",
                "some-black-listed-artists.txt",
            ),
            (
                ["source_a"],
                "single_char",
                "some-white-listed-artists.tus.tm",
                "some-black-listed-artists.tus.tm",
            ),
            (
                ["source_1", "source_2", "source_3"],
                "char",
                "some-white-listed-artists-01.csv",
                "some-black-listed-artists-01.csv",
            ),
            (
                ["source_6", "source_5", "source_4"],
                "op_num",
                "some-white-listed-artists",
                "some-black-listed-artists",
            ),
        ],
    )
    @pytest.mark.asyncio
    async def test_query_for_music_search_whitelisted_artist_s3_key_and_blacklisted_s3_key_duplicate(
        self,
        redis_test_connector: RedisConnector,
        whitelisted_source: List,
        term: str,
        whitelisted_artist_s3_key: str,
        blacklisted_artist_s3_key: str,
        monkeypatch,
    ):
        _whitelisted_fuzz_score = 1
        _blacklisted_fuzz_score = 2
        country_code: str = "us"
        filter_config: MusicFilterConfig = MusicFilterConfig(
            whitelisted_source=whitelisted_source,
            whitelisted_artist_s3_key=whitelisted_artist_s3_key,
            blacklisted_artist_s3_key=blacklisted_artist_s3_key,
            whitelisted_fuzz_score=_whitelisted_fuzz_score,
            blacklisted_fuzz_score=_blacklisted_fuzz_score,
            auth={"region": "us-east-1"},
        )
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )

        # return same blacklisted and whitelisted artist
        async def redis_cached_artists(*args, **kwargs):
            assert kwargs == {
                "name": _ms.generate_filtered_artists_redis_key(
                    whitelisted_artist_s3_key
                )
            } or {
                "name": _ms.generate_filtered_artists_redis_key(
                    blacklisted_artist_s3_key
                )
            }
            return ["some-artist-1", "some-artist-2"]

        monkeypatch.setattr(
            redis_test_connector,
            "operate",
            redis_cached_artists,
        )
        generated_query = await _ms.query(term=term)

        output_query_json = {
            "_source": "false",
            "query": {
                "bool": {
                    "must": [
                        {
                            "match": {
                                "music_search_field": {
                                    "query": term,
                                    "fuzziness": "AUTO",
                                }
                            }
                        },
                        {
                            "bool": {
                                "should": [
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "terms": {
                                                        "source": whitelisted_source
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must_not": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-artist-1",
                                                            "fuzziness": _blacklisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must_not": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-artist-2",
                                                            "fuzziness": _blacklisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                ]
                            }
                        },
                    ],
                    "filter": {
                        "bool": {
                            "must": [
                                {"term": {"status": "active"}},
                                {
                                    "bool": {
                                        "should": [
                                            {
                                                "bool": {
                                                    "must_not": [
                                                        {"term": {"source": "7digital"}}
                                                    ]
                                                }
                                            },
                                            {
                                                "bool": {
                                                    "must": [
                                                        {
                                                            "term": {
                                                                "source": "7digital"
                                                            }
                                                        },
                                                        {
                                                            "term": {
                                                                "country_code": country_code.lower()
                                                            }
                                                        },
                                                    ]
                                                }
                                            },
                                        ]
                                    }
                                },
                            ]
                        }
                    },
                }
            },
        }
        assert output_query_json == generated_query

    @pytest.mark.parametrize(
        "whitelisted_source, term, whitelisted_artist_s3_key, blacklisted_artist_s3_key, country_code",
        [
            (
                ["source_a", "source_b", "source_c"],
                "char",
                "some-white-listed-artists.txt",
                "some-black-listed-artists.txt",
                "us",
            ),
            (
                ["source_a"],
                "single_char",
                "some-white-listed-artists.tus.tm",
                "some-black-listed-artists.tus.tm",
                "in",
            ),
            (
                ["source_1", "source_2", "source_3"],
                "char",
                "some-white-listed-artists-01.csv",
                "some-black-listed-artists-01.csv",
                "gb",
            ),
            (
                ["source_6", "source_5", "source_4"],
                "op_num",
                "some-white-listed-artists",
                "some-black-listed-artists",
                "ts",
            ),
        ],
    )
    @pytest.mark.asyncio
    async def test_query_for_music_search_whitelisted_artist_s3_key_and_blacklisted_s3_key_unique(
        self,
        redis_test_connector: RedisConnector,
        whitelisted_source: List,
        term: str,
        whitelisted_artist_s3_key: str,
        blacklisted_artist_s3_key: str,
        country_code: str,
        monkeypatch,
    ):
        _whitelisted_fuzz_score = 1
        _blacklisted_fuzz_score = 2
        filter_config: MusicFilterConfig = MusicFilterConfig(
            whitelisted_source=whitelisted_source,
            whitelisted_artist_s3_key=whitelisted_artist_s3_key,
            blacklisted_artist_s3_key=blacklisted_artist_s3_key,
            whitelisted_fuzz_score=_whitelisted_fuzz_score,
            blacklisted_fuzz_score=_blacklisted_fuzz_score,
            auth={"region": "us-east-1"},
        )
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )

        # return same blacklisted and whitelisted artist
        async def redis_cached_artists(*args, **kwargs):
            assert kwargs == {
                "name": _ms.generate_filtered_artists_redis_key(
                    whitelisted_artist_s3_key
                )
            } or {
                "name": _ms.generate_filtered_artists_redis_key(
                    blacklisted_artist_s3_key
                )
            }

            if kwargs == {
                "name": _ms.generate_filtered_artists_redis_key(
                    blacklisted_artist_s3_key
                )
            }:
                return ["some-blacklisted-1", "some-blacklisted-2"]

            if kwargs == {
                "name": _ms.generate_filtered_artists_redis_key(
                    whitelisted_artist_s3_key
                )
            }:
                return ["some-whitelisted-1", "some-whitelisted-2"]

        monkeypatch.setattr(
            redis_test_connector,
            "operate",
            redis_cached_artists,
        )
        generated_query = await _ms.query(term=term)

        output_query_json = {
            "_source": "false",
            "query": {
                "bool": {
                    "must": [
                        {
                            "match": {
                                "music_search_field": {
                                    "query": term,
                                    "fuzziness": "AUTO",
                                }
                            }
                        },
                        {
                            "bool": {
                                "should": [
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "terms": {
                                                        "source": whitelisted_source
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must_not": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-blacklisted-1",
                                                            "fuzziness": _blacklisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must_not": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-blacklisted-2",
                                                            "fuzziness": _blacklisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-whitelisted-1",
                                                            "fuzziness": _whitelisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                    {
                                        "bool": {
                                            "must": [
                                                {
                                                    "match": {
                                                        "artist_display_name": {
                                                            "query": "some-whitelisted-2",
                                                            "fuzziness": _whitelisted_fuzz_score,
                                                        }
                                                    }
                                                }
                                            ]
                                        }
                                    },
                                ]
                            }
                        },
                    ],
                    "filter": {
                        "bool": {
                            "must": [
                                {"term": {"status": "active"}},
                                {
                                    "bool": {
                                        "should": [
                                            {
                                                "bool": {
                                                    "must_not": [
                                                        {"term": {"source": "7digital"}}
                                                    ]
                                                }
                                            },
                                            {
                                                "bool": {
                                                    "must": [
                                                        {
                                                            "term": {
                                                                "source": "7digital"
                                                            }
                                                        },
                                                        {
                                                            "term": {
                                                                "country_code": country_code.lower()
                                                            }
                                                        },
                                                    ]
                                                }
                                            },
                                        ]
                                    }
                                },
                            ]
                        }
                    },
                }
            },
        }
        assert output_query_json == generated_query

    @pytest.mark.parametrize(
        "key, country_code",
        [
            (
                "some-white-listed-artists.txt",
                "us",
            ),
            ("some-white-listed-artists", "in"),
            ("some-black-listed-artists", "IN"),
            (
                "some-black-listed-artists.txt.cst",
                "ts",
            ),
            (".txt.cst", "GB"),
        ],
    )
    @pytest.mark.asyncio
    async def test_getting_empty_filter_artists_when_s3_not_available(
        self,
        redis_test_connector: RedisConnector,
        key: str,
        country_code: str,
        monkeypatch,
    ):
        filter_config: MusicFilterConfig = MusicFilterConfig()
        _ms: MusicSearch = get_testable_music_search(
            redis_test_connector=redis_test_connector,
            filter_config=filter_config,
            country_code=country_code,
        )
        assert await _ms.get_filtered_artists(key) == []
