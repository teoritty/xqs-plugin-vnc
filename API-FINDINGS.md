# API-FINDINGS

Находки по Plugin API xQuakShell, обнаруженные при проектировании первого сложного плагина (VNC).
Формат: что ожидалось по докам → что в коде → предложение.

Источник: `C:\Users\fedor\Desktop\My\xQuakShell\xQuakShell` (read-only), ADR-008/011.
Дата первого прохода: 2026-07-16. Все ссылки на строки в исходных пунктах F-1..F-7 — на момент того чтения.

**Обновление 2026-07-17:** перечитано после серии коммитов владельца ядра (см. §«Ревизия 2026-07-17» в конце
каждого пункта). F-1, F-2, F-3, F-4 закрыты. F-5 подтверждён как дефект ADR, не кода. F-6 закрыт кодом,
частично. F-7 остаётся открытым. Новый пункт F-8: фактический потолок фрейма `embed-stream` — 64 KiB, не 1 MiB.

---

## F-1 (БЫЛ БЛОКЕР, ЗАКРЫТ 2026-07-17). Плоскость данных канальной шины не подключена в композиционном корне

**Ожидалось:** ADR-011 — плагин открывает канал и гонит по нему `kind=0x02`; хост терминирует дальний конец.

**В коде:**

1. `internal/infra/plugin/capability/channel_proxy.go:145` — `ChannelHandle` конструируется **без `Data`**:
   ```go
   handle := &domainplugin.ChannelHandle{
       ChannelID: id, PluginID: p.pluginID, Purpose: req.Purpose,
       ParentSessionID: req.ParentSessionID, Hint: req.Hint,
   }   // Data не установлен
   backend.Wire(ctx, handle)
   ```
   Все четыре бэкенда (`channel_relay_backend.go:156`, `channel_embed_backend.go:129`,
   `channel_udp_relay_backend.go:167`, `channel_exec_backend.go`) начинаются с
   `if ch.Data == nil { return nil }` → `Wire` выходит немедленно, горутины перекачки не стартуют.

   `internal/domain/plugin/channel_port.go:43` фиксирует это прямо:
   «Data is nil until the composition root wires the channel's real data path to it».
   Композиционный корень этого не делает.

2. `internal/infra/plugin/ipc/channel_mux.go:18` — `Register(id uint32)` **не вызывается в проде**.
   (`process_host_start.go:73` — другой, двухаргументный `ChannelBus.Register(key, channelProxy)`.)
   Мультиплексор не знает ни одного канала → первый же `kind=0x02` от плагина:
   `Dispatch` → `newProtocolViolation("frame kind 0x02 for unknown channel id")`
   → `Conn.failReadLoop` → соединение закрыто → процесс убит → crash-recovery ×3 → сессия в `error`.

**Наблюдаемое поведение:** `channel.open` успешно возвращает `channelId`; на первом бинарном фрейме
плагин убивается. Байты не текут ни при каком раскладе.

**Почему не поймали тесты:** каждый уровень покрыт на фейках — `fakeChannelDataPath` в тестах
бэкендов, `newFlowChannel` в тестах ipc. Шов между ними (адаптер `ipc.channel` → `ChannelDataPath`)
не существует, и тестировать в нём нечего. Все юниты зелёные, системы нет.

**Предложение:** адаптер в композиционном корне: на `channel.open` — `mux.Register(id)` (через
`newFlowChannel(id, purpose, initialCredit(purpose), maxThroughputKbps, clk, conn.WriteBinary…)`),
обернуть результат в `ChannelDataPath` и положить в `handle.Data` до `backend.Wire`. Плюс
интеграционный тест «плагин → байт → бэкенд», который по построению нельзя пройти на фейках.

**Ревизия 2026-07-17:** реализовано ровно предложенным способом, с усилением. `channel_proxy.go`
больше не строит `ChannelHandle` напрямую — `internal/infra/plugin/ipc/channel_data_path.go` вводит
`Conn.OpenDataPath(id, purpose) (ChannelDataPath, error)`, которую комментарий кода называет «the
composition root's single entry point into the bus, and the reason a ChannelHandle can no longer be
built without one». Внутри — ровно `mux.Register` → обёртка `channelDataPath` в `handle.Data`, до
`backend.Wire`. `OpenDataPath` также рубит открытие канала с неизвестным purpose на старте
(`ErrUnknownChannelPurpose`), а не молча даёт нулевое окно. Интеграционные тесты на реальный `Conn`
добавлены (`channel_seam_test.go`). **Статус: закрыт, не обход — структурный фикс, который делает
регресс невозможным по типам** (нельзя сконструировать `ChannelHandle` без прохода через `OpenDataPath`).

---

## F-2. Stage 5 (flow control) — мёртвый код, следствие F-1

`newFlowChannel` (`ipc/channel.go:65`) вызывается **только из тестов**. Единственный продовый путь —
`channelMux.Register` → `newChannel(id)` → `credit == nil`.

При `credit == nil`:
- `Send` (`channel.go:98`): `if c.credit == nil { return c.writeOut(payload) }` — без кредита,
  без токен-бакета, без drop-oldest;
- `deliver` (`channel.go:149`): `kind == FrameKindBinary && c.credit != nil` → false → без
  `ConsumeInbound`, без учёта;
- очередь inbound — **неограниченная** по прямому комментарию `channel.go:25-28`
  («the queue is unbounded»).

Следствия: `GrantInbound` (`channel_credit.go:144`) и `Conn.WriteCredit` (`conn.go:112`) не имеют
продовых вызовов; `maxThroughputKbps` объявлен, но не применяется — ровно то, чего ADR-011
хотел избежать («A manifest field with no corresponding runtime check would be worse than no field
at all»).

**Предложение:** решается тем же адаптером из F-1. Отдельно стоит проверить, кто эмитит `kind=0x03`
в сторону плагина: `GrantInbound` документирован как «the caller is responsible for emitting the
matching kind=0x03 frame», но такого caller'а нет.

**Ревизия 2026-07-17:** закрыт вместе с F-1. `newFlowChannel` теперь единственный продовый путь
конструирования канала (`channelDataPath.Ack` вызывает `p.ch.grantInbound(1)` и шлёт `kind=0x03`
через `conn.WriteCredit` — тот самый недостающий caller). Токен-бакет и drop-oldest — живой код.

---

## F-3. Семантика пополнения кредита не определена

`channel_credit.go:142`: «GrantInbound increases the credit granted to the plugin, **e.g. as the host
drains/processes received frames**». ADR-011 §2b — та же формулировка «as it drains/processes».

«drains/processes» не различает два принципиально разных момента:

| Момент выдачи `kind=0x03` | Следствие для плагина |
|---|---|
| после слива в WebSocket | кредит = прокси потребления браузером; плагин может связать окна и не допускать дропов |
| после приёма в буфер хоста | кредит меряет скорость хоста; буфер растёт и дропает независимо от окна плагина — **средств защиты у плагина нет** |

Актуально станет после F-1. **Предложение:** зафиксировать момент в ADR-011 явно.

**Ревизия 2026-07-17: закрыт, выбран правильный для RFB вариант.** Коммит `531a257` («ack a tunnel
frame when the socket takes it, not the queue») пинит именно этот вопрос: `Ack` (и, значит,
`kind=0x03`) теперь эмитится по факту `ws.WriteMessage` возврата, а не по факту постановки в очередь
хоста — то есть строка таблицы «после слива в WebSocket» из этого пункта, не «после приёма в буфер
хоста». Очередь на хосте физически не может превысить кредитное окно `embed-stream` (8 фреймов ×
64 KiB, см. F-8) — до этого коммита она держала до 256 фреймов (16 MiB) неучтёнными окном. Кредит
теперь честно прокси реального потребления браузером — связка окон из §3 плана плагина защищает
именно то, для чего задумана.

---

## F-4. Drop-oldest на `embed-stream` несовместим с инкрементальными протоколами

ADR-011 формулирует намерение прямо: «`exec` channels should *not* silently drop bytes,
**`embed-stream` channels should**»; `channel_backpressure.go:20-23`: «latest-frame-wins…
There is no upstream to pause — this is host-side buffer logic only».

Политика корректна для видео (каждый кадр самодостаточен) и неверна для RFB: `FramebufferUpdate` —
дельта поверх предыдущего состояния. Выброшенная дельта = перманентный артефакт до следующего
обновления региона (для статики — навсегда). Выброс фрейма из середины многофреймовой дельты =
потеря границ сообщений и неустранимый рассинхрон noVNC. Ошибки при этом не возникает — портится
картинка.

**Уточнение направления:** `policyDropOldestUnsent` применяется к **outbound**-кредиту, то есть к
направлению **хост → плагин**. Для `embed-stream` это ввод из браузера (`SubscribeOutbound` →
`data.Send`), а не фреймбуфер. Дропать ввод (нажатия клавиш) — тоже неверно, но объём там мал.
Фреймбуфер идёт плагин → `data.Recv()` → `RouteTunnelFrameFromPlugin`, и его судьба зависит от F-3.

**Предложение:** (а) режим backpressure для `embed-stream` наравне с `tcp-relay`, чтобы drop-oldest
стал опцией, а не единственной политикой; (б) как минимум — предупреждение в ADR-008/011, что
latest-frame-wins несовместим с инкрементальными протоколами.

**Ревизия 2026-07-17: закрыт вариантом (а), причём сильнее — drop-oldest для `embed-stream` больше
не единственная политика на пути плагин → браузер.** Два коммита меняют дисциплину:

- `061adcc` («stop discarding plugin frames for an unattached tunnel») различает три состояния
  отсутствия WS-потребителя (ещё не подключён / закрыт / никогда не существовал) вместо молчаливого
  `nil` = «доставлено». Раньше кредит плагина открывался, а дельта уничтожалась без единого сигнала
  на любом уровне — ровно сценарий, которого боится этот пункт.
- `3319303` («stop tearing down a channel a lagging browser can still drain») меняет реакцию на
  «буфер занят»: раньше любой отказ синка убивал канал; теперь `Ack` просто не эмитится, пока фрейм
  не принят на том конце — окно плагина закрывается, backpressure доходит до VNC-сервера через
  связку §3 плана. Фрейм на руках **никогда не дропается**. Есть потолок ожидания `embedAckCeiling`
  (120 с) для мёртвого потребителя, и он **не тратится**, пока вкладка просто в фоне (`tunnelBackpressure`
  для свёрнутой вкладки — не то же самое, что мёртвый браузер).

Итог: путь плагин → браузер (фреймбуфер, то самое, чего боялся этот пункт) для `embed-stream`
теперь **настоящий backpressure**, не drop-oldest. Latest-frame-wins остаётся политикой пути
браузер → плагин (ввод), где потеря клика/символа некритична и объём мал — ровно уточнение
направления, которое этот пункт сам сформулировал в 2026-07-16. RFB-дельты больше не могут быть
безвозвратно повреждены исчерпанием кредита на этом плече.

---

## F-5. ADR-011 §3 противоречит коду по dial-политике `tcp-relay`

| Источник | Утверждение |
|---|---|
| ADR-011 §3 | «Falls back to the existing `TunnelDialProxy` dial policy (**host:port allowlist, no wildcards**)» |
| `plugin-manifest.md`, `security-model.md` | «reuses the existing dial policy **verbatim**» |
| **Код** (`channel_relay_backend.go:62-83`) | `allowArbitrary = b.caps.AllowArbitraryOutbound`; отказ только при `!allowArbitrary && !allowlistAllowsHost` |

Правы доки, неправ ADR: arbitrary-режим для `tcp-relay` поддержан. Существенно, потому что
аллоулист-онли сделал бы `tcp-relay` непригодным для протоколов с пользовательским хостом (VNC/RDP).

**Предложение:** поправить формулировку ADR-011 §3.

**Ревизия 2026-07-17:** без изменений, дефект подтверждён повторным чтением
(`channel_relay_backend.go`, `docs/adr/011-binary-channel-bus.md`). Не влияет на наш манифест —
`allowArbitraryOutbound: true` был и остаётся верным выбором (§6 плана).

---

## F-6. Стык ADR-008 и ADR-011 не описан

ADR-008 (ядро `0.3.0-dev`) знает только `registerEmbed` → `tunnelOpen` → `tunnelFrame` и про каналы
не знает. ADR-011 добавляет `embed-stream` одной строкой («Host wires that channel directly to the
embed surface's video pipe») и не говорит, что происходит с туннелем ADR-008.

**Ответы, вычитанные из кода** (`channel_embed_backend.go`) — их стоит перенести в доки:

- `session.registerEmbed` **обязателен и первичен**: `Authorize` требует
  `PluginIDForSession(parentSessionID) == pluginID`; «a channel is never itself how embed
  ownership is established».
- `hint` несёт `tunnelId`; пустой → `"main"` (`embedDefaultTunnelID`).
- Оба направления идут по одному каналу; `session.tunnelData` на канальном пути не используется.
- `parentSessionId` собственной embed-сессии принимается — отдельной родительской сессии не нужно.

**Осталось неясным:** нужен ли `session.tunnelOpen` при канальном пути; применимы ли
`tunnelBackpressure`/`tunnelResume` к каналам или только к legacy-пути.

**Ревизия 2026-07-17: второй вопрос закрыт кодом, первый остаётся открытым.** `061adcc` вводит
`ChannelCloseNotifier`, подключаемый из композиционного корня (`AttachChannelCloseNotifier`) именно
для канального пути — `tunnelBackpressure`/`session-revoked` теперь долетают до `embed-stream`
backend'а тем же механизмом, что и до legacy `tunnelFrame`, а не только до него. Про
`session.tunnelOpen` при канальном пути документация по-прежнему молчит; по коду (`registerEmbed` +
`channel.open` с `hint.tunnelId`, без отдельного открытия туннеля) похоже, что он не нужен, но это
предположение, не подтверждённое явно — как и было.

---

## F-7. Как embed-страница узнаёт `tunnelUrl` — не описано

`SessionEmbedReady` отдаёт `uiUrl`/`tunnelUrl` фронтенду хоста. Получает ли их сама страница в
iframe — не задокументировано. Вероятно выводится из `location`
(`/embed/s/<token>/ui/…` → `/embed/s/<token>/tunnel/main`), но это допущение.

**Предложение:** описать контракт явно (postMessage от хоста или гарантия вывода из пути).

**Ревизия 2026-07-17:** без изменений. Остаётся открытым — не задет ни одним из коммитов ядра за
2026-07-17. Уточняется на месте, эмпирически, в начале фазы 3 (`ui/boot.js`).

---

## F-8 (новое, 2026-07-17). Потолок фрейма `embed-stream` — 64 KiB, не 1 MiB

**Найдено при ревизии, не в исходном проходе.** ADR-011 и `plugin-manifest.md` документируют общий
потолок бинарного канала в 1 MiB (`MaxBinaryFrameBytes`). До коммита `e33a9e8` это было ложью для
`embed-stream`: заголовок фрейма проходил валидацию на 1 MiB, но `channel.deliver` отбивал всё, что
крупнее 64 KiB, кодом `ErrRateLimited` — который embed-бэкенд трактовал как фатальный отказ.
Итог тогда: плагин, честно соблюдающий документированный лимит, убивался на первом же кадре крупнее
64 KiB.

**После `e33a9e8`:** явный отдельный потолок `MaxTunnelFrameSize = 64 KiB` для purpose `embed-stream`,
проверяется в `channel.deliver` — первой точке, где известны и purpose, и payload. `MaxBinaryFrameBytes`
(1 MiB) остаётся общим потолком для остальных purpose (`tcp-relay`, `exec`, `udp-relay`). Лимит
осмыслен и назван load-bearing: кредит считается фреймами, и окно 8 × 1 MiB держало бы 8 MiB на канал
в памяти хоста, которую Job Object плагина не ограничивает; 8 × 64 KiB = 512 KiB — совпадает с числом,
на которое рассчитан ADR-011.

**Значение для плана плагина:** §3, §6, §7 плана (`docs/superpowers/specs/2026-07-16-vnc-plugin-design.md`)
говорят «потолок фрейма — 1 MiB» применительно к обеим трубам. Для `tcp-relay` это верно. Для
`embed-stream` — нет, верно 64 KiB. Резка RFB-дельт по границам сообщений (§3, §7 «Размер фрейма») должна
целиться в 64 KiB, а не в 1 MiB — иначе первый же кадр крупнее лимита убивает плагин тем же классом
ошибки, который F-1..F-4 уже устранили на уровне транспорта. Правка внесена в план, см. §3/§6/§7 там.

---

## Общая закономерность

ADR-011 проектировался с примером «video» в голове. F-4 и F-5 растут из одного корня:
**инкрементальный протокол к пользовательскому хосту — это не видеопоток к известному эндпоинту.**
Latest-frame-wins, «плагину не нужна drop-логика», аллоулист вместо пользовательского хоста —
каждое решение разумно для видео и ломается на RFB.

F-1 — иного рода: не проектная ошибка, а незаконченная проводка, которую не мог поймать ни один
юнит-тест, потому что все они честно тестируют свои уровни на фейках.

---

## Ревизия 2026-07-17 — сводка

Все находки перечитаны против текущего `main` ядра (последний релевантный коммит `09dafc3`).
Владелец ядра закрыл F-1 через F-4 и частично F-6 за один рабочий день (13:02–13:39, 2026-07-17),
целенаправленно — коммиты именуют предыдущие находки как «D2», «D4», «D5», «D6», «D10», то есть
велись по тому же разбору, что и `API-FINDINGS.md`. Итоговый статус:

| # | Было | Стало |
|---|---|---|
| F-1 | Блокер: data plane не подключена | **Закрыт.** `OpenDataPath` — типобезопасный шов |
| F-2 | Мёртвый код, следствие F-1 | **Закрыт вместе с F-1** |
| F-3 | Момент кредита не определён | **Закрыт.** Кредит — по факту `ws.WriteMessage` |
| F-4 | Drop-oldest ломает RFB-дельты | **Закрыт.** Реальный backpressure на плечо плагин→браузер |
| F-5 | ADR-011 §3 против кода (dial policy) | Подтверждён, не тронут. Наш манифест не задет |
| F-6 | Стык ADR-008/011 не описан | Частично закрыт (`tunnelBackpressure` на канальном пути) |
| F-7 | `tunnelUrl` для iframe не описан | Открыт |
| F-8 | *(новое)* лимит фрейма `embed-stream` = 64 KiB, не 1 MiB | Задокументировано, план поправлен |

**Что это значит для продуктовой задачи.** Исходная рамка была «первый сложный плагин на этом API,
разведка окупилась до первой строки кода». Теперь она шире: коммит `cffd564` («raise
MaxTunnelsPerSession to 8 for multi-channel protocols») прямо называет причину — «4 fits VNC and
nothing else... the old ceiling... is what would make this API VNC-only. 8 covers [SPICE and RDP]
with headroom». Канальная шина явно спроектирована и протестирована (`embed_tunnel_multi_test.go`,
per-tunnel isolation) не только под VNC, а под произвольные многоканальные графические протоколы.
Блокер, из-за которого задачу поставили на паузу, снят; препятствий уровня ядра для запуска фазы 3
VNC-плагина по плану `docs/superpowers/specs/2026-07-16-vnc-plugin-design.md` больше нет, за
вычетом F-7 (не блокер — уточняется эмпирически) и открытого вопроса про `session.tunnelOpen`
в F-6.
