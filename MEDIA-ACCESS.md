# Media delivery authorization

Configure `MEDIA_ACCESS_URL` with the application's CID policy endpoint and
`MEDIA_LINK_ACCESS_URL` with its placement endpoint. The IAMFree endpoints are
`/api/media_delivery` and `/internal/api/media_delivery_link`. The latter must
be reachable from Storage; it is not a browser API.

Deploy the matching API and local upload-preview UI changes before this Storage
version. The API explicitly authorizes saved chat/support attachments and
registered unplaced uploads for their owner. Before registration, the form
previews the selected local bytes; unknown CIDs are never made public to support
that preview. An empty
policy, inaccessible placement, or unknown resource now returns **403**; a
failed policy dependency returns **503**. Neither result allows original bytes.

An absent or invalid policy configuration also fails closed. A standalone
installation that intentionally has no per-file ACLs must explicitly set
`MEDIA_ALLOW_UNMANAGED=true` and leave both policy URLs empty. Do not enable
that mode on a protected IAMFree deployment.

The policy response binds a placement to `storage_uri`, `poster_uri` and a
delivery mode. A browser cannot apply a public placement to a different CID.
Legacy HLS playlists carry `media_root`; Storage validates their children
against the immutable master/variant playlists. A client-supplied
`metadata.stream_cids` list is not proof of membership. Opaque indexed HLS URLs
remain supported and avoid scanning the compatibility tree.

Both `blur` and `blur_faces` prohibit original video streams, including direct
segments and requests disguised with an image extension. Only an available
protected poster is delivered. Missing protected renditions fail closed.

Protected responses use `Cache-Control: private, no-store`. Check reverse proxy
cache rules on deployment: an old response previously marked public is not
retroactively invalidated by these headers. Invalidate any affected shared
cache entries if such a cache was enabled.

This change fixes authorization using online policy decisions. It does not
implement signed download grants or remove the API policy call per download.

The stand check additionally covered actual private-photo bytes, verification
owner/agency/staff, protected video posters, HLS master/variant/init/media segments,
and direct-CID substitution. Denials also carry private/no-store headers.
Before deployment, audit legacy raw avatar references as described by the API
change: references to another profile's restricted library do not grant access.

## What `fix/media-rate-limit` adds on top

These changes were made on the IAMFree stand and merged with the contract
above (fail closed, placements bound to `storage_uri`/`poster_uri`).

- **A `media_link` speaks only for its own file.** When the policy answer for
  the link names another source than the CID in the path, the link is set
  aside and the file is judged by its CID alone (`mediaAccessResolver.Resolve`).
- **Deny is a refusal, not an outage.** A `deny` row, an empty answer and
  401/403/404 from the policy all answer **403** (`errMediaForbidden` is the
  same error as `errMediaAccessDenied`); only a failed dependency is 503.
- **Video with a hidden face.** `blur` and `blur_faces` both refuse the master,
  playlists and segments; the refusal says *Private video is unavailable* or
  *Video with a hidden face is unavailable* (`mediaDeliveryHidesStream`). The
  protected poster the master binds to the mode stands in for the video.
- **Redrawn face masks.** A mask is drawn at upload and kept in the bundle,
  whose address cannot change. `POST /admin/face-masks/refresh` (platform API
  key only) redraws masks of photo bundles and video posters from the
  originals and records each replacement in `/data/variant-overrides.json`
  on the instance's volume; `overriddenFile` serves the replacement in place
  of the bundled mask, falling back to the bundled one if it cannot be read.
  The override is applied to the poster the master binds, never to one named
  by editable metadata.
- **Uploads.** HEIC stills are converted to JPEG at ingest; EXIF (with GPS),
  XMP, IPTC and comments are stripped from JPEG/PNG/WebP originals (a JPEG
  keeps its orientation); a session uploads at most
  `UPLOAD_DAILY_BYTES_PER_SESSION` a day per instance (4 GiB by default); a
  multi-file upload carries at most 20 files; a picture over 100 megapixels is
  refused before it is decoded; videos wait for one of two transcoding slots
  (five minutes at most), a transcode stops after twenty minutes, frames over
  4096 px a side or 120 fps are refused; a video of any shape is accepted and
  its poster is scaled inside the variant box.
- **Stored files never act as pages.** A CID is a CID and nothing after it;
  stored files are served with a sandboxing CSP and `nosniff`, HTML and SVG
  uploads are refused, and `DELETE /file` takes the platform API key only. The
  session may come in the `iamfree_media_token` cookie instead of the query.
- **Bursts.** The rate limit allows the bursts of media-heavy pages.

### Stand configuration after the merge

`MEDIA_ACCESS_URL` names the API's `/api/media_delivery` (it answers chat and
support attachments too, and the unregistered pictures a record names: an
agency's or a manager's avatar from «About me» and a tour's cover).
`MEDIA_LINK_ACCESS_URL` stays `/internal/api/media_delivery_link`. Before the
merge this storage read the API branch's own `/api/media_delivery_cid`, which
answered an unplaced file with no rows; the merged storage refuses that, and
the API has removed the endpoint.

The policy read takes about 0.1 s once the API's database connection sends
`jit=off`; with JIT, compiling it took about 1.8 s, close to
`MEDIA_ACCESS_TIMEOUT_MS` (2500), and avatars answered 503.

## Размеры защищённых изображений (12.10.2026)

`/file/{cid}/{variant}` и `/file/link/{id}/{variant}` применяют размеры из
`IMAGE_VARIANTS` к разрешённой версии изображения, включая `blur` и `blur_faces`.
Crop сохраняет настроенную геометрию, fit — весь кадр в заданных пределах.
Явный `/{variant}/blur` не ослабляет более строгую маску политики/grant.
Производные файлы создаются один раз и сохраняются через variant overrides;
ключ включает размер, режим, качество и CID перерисованной маски.
Права и отзыв grant проверяются при каждом запросе, HTTP остаётся private/no-store.

Клиент IPFS использует ограниченный пул свободных HTTP-соединений вместо
отключённого в стандартном клиенте Kubo keep-alive. Чтение по CID и пути
распределяется между узлами с последовательным переходом к другим при ошибке.

## Публичные медиапакеты

Фильмы публичных сайтов платформы — единственное, что хранилище отдаёт без
учётной записи. Это отдельный узкий режим, а не ослабление модели выше:
`MEDIA_ALLOW_UNMANAGED` остаётся `false`, `/file/` и `/stream/` по-прежнему
требуют сессии и политики. Без `PUBLIC_MEDIA_PACKAGES` режим выключен, и
`/public/` уходит в обычный роутер (401).

**Что раздаётся.** Только каталоги, названные в `PUBLIC_MEDIA_PACKAGES`
(`имя=CID каталога`). По имени открываются только `master.m3u8` и `poster.jpg`.
Варианты, init, сегменты, плейлисты субтитров и `.vtt` — только по ссылкам,
которые хранилище само вписывает в плейлисты при каждой выдаче:
`/public/{name}/l/{payload}.{mac}/{файл}`. `payload` — версия, срок и путь;
`mac` — HMAC-SHA256 (16 байт) ключом `PUBLIC_MEDIA_SIGNING_KEY` по имени, CID,
пути и сроку. Срок — `PUBLIC_MEDIA_LINK_TTL` (45 мин, округление до минуты).
Путь проверяется: относительный, без `..`, не глубже 4 уровней, только типы
`m3u8 mp4 m4s m4a aac ts vtt jpg webp png`. Плейлист, ссылающийся на абсолютный
адрес, за пределы каталога, на ключ шифрования или неизвестный тип, не отдаётся
(502). Каталогов, листинга и записи нет; CID неизменяем.

**Какому сайту.** Решение по заголовкам браузера:

| Запрос | Ответ |
|---|---|
| `Origin` из `PUBLIC_MEDIA_ORIGINS` (hls.js: все запросы — XHR/fetch) | 200, CORS для этого сайта |
| `Origin` другой, `null`, http-вариант | 403 |
| без `Origin`, `Referer` с разрешённого сайта | 200 |
| `Referer` с другого сайта | 403 |
| `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document/iframe/…` (адрес открыт во вкладке, фрейм) | 403 |
| межсайтовый (`Sec-Fetch-Site: cross-site`) без `Origin` и `Referer` | 403 |
| ни `Origin`, ни `Referer`, ни `Sec-Fetch-Site` | 200 при `PUBLIC_MEDIA_ALLOW_NO_ORIGIN=true` |

Последняя строка — компромисс. Нативный HLS Safari/iOS (AVFoundation) запрашивает
плейлисты и сегменты без `Origin`, а иногда и без `Referer`; без этой строки
ролик на iPhone не играет. Так же выглядит `curl`: тот, кто скопировал адрес
master, получит фильм по сегментам, пока живут ссылки. Защита рассчитана на
обычного посетителя (нет кнопки скачивания, нет единого mp4, ссылки истекают,
чужие сайты не встроят ролик), а не на целенаправленное копирование — это
не DRM. `false` закрывает и этот путь ценой Safari без MSE (iPhone до iOS 17.1).

**Истечение.** Ссылка истекла или подписана для прежнего CID → 403 `Link expired`
/ `Invalid link` с CORS для разрешённого сайта: плеер видит ответ и заново
открывает `master.m3u8` с той же позиции (так делает плеер сайта).

**Кэш.** Плейлисты `private, no-store` (в них истекающие ссылки), сегменты
`private, max-age` не дольше оставшегося срока ссылки (≤ 1 ч), постер
`public, max-age=3600`. Общий кэш (Cloudflare) не должен хранить ответы
`/public/` с подписью: адреса `sNNN.media.…` не проксируются Cloudflare,
а HAProxy перед хранилищем всё равно ставит `private, no-store`.

**Нагрузка.** Плейлисты пакетов кэшируются в памяти (до 256), сегменты читаются
из IPFS потоком. Отдельный лимит на IP: `PUBLIC_MEDIA_RATE_LIMIT_RPS=20`,
`PUBLIC_MEDIA_RATE_LIMIT_BURST=80`. Ключ подписи одинаков на всех инстансах,
которые раздают пакеты; его смена гасит выданные ссылки (плеер восстановится).
