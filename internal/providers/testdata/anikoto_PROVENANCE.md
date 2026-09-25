anikoto testdata fixtures — REAL captures from https://anikototv.to (2026-09-25):
- anikoto_search.html: GET /filter?keyword=black lagoon (HTTP 200, 30 result cards)
- anikoto_watch.html: GET /watch/black-lagoon-the-second-barrage-omdia/ep-1 (HTTP 200)
- anikoto_episodes.json: GET /ajax/episode/list/1359?vrf= (X-Requested-With: XMLHttpRequest)
- anikoto_servers.json: GET /ajax/server/list?servers=<ep1 data-ids>
- anikoto_stream.json: GET /ajax/server?get=<Vidstream-2 link-id> (megaplay url + skip_data)
- anikoto_megaplay.html: GET the megaplay.buzz stream url (player page, data-id 139330)
- anikoto_e1player.js: GET megaplay.buzz/lib/e1-player.min.js?v=2.20assdsdsad (XOR-obfuscated)
- anikoto_getsources.json: GET megaplay.buzz/stream/getSourcesNew?id=139330 (enc blob + tracks)
- anikoto_master.m3u8: GET the decrypted+HMAC-signed master playlist (variants 1080/720/480)
