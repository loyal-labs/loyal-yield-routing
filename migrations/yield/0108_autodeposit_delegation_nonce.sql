-- A target's recurring delegation is the PDA of (subscription authority,
-- wallet, vault, nonce): an address without its nonce cannot be proven, so
-- every reader had to treat it as unknown and the target never deposited.
-- 44 production targets held a delegation address without its nonce. Each
-- row below is that target's confirmed create_recurring instruction, decoded
-- from chain: nonce, start, expiry, signature and slot. Start and period equal
-- the stored values wherever one was stored; target 26 had no start.
SET LOCAL lock_timeout = '5s';

WITH observed (id, recurring_delegation, nonce, start_timestamp, expiry_timestamp, signature, slot) AS (
    VALUES
    (26, '9r5XsVedFjudTqFvw3htG8TRTdZGWcJvNKQ51fo7VAz1', 1781059957588, 1781059959, 0, '369ESfEM4FKfRwoAACuoYt42byreoQKxLRmNqd8ggFiB8vHwAxGLTuAgU1LadTHmUqKf77mqvQdkwq2MNBg2Qjqw', 425458482),
    (29, 'DK8LfEXB1vawduih3BuVwyCHy5GFcJJhwEUc6HKzVNDc', 1781160607939, 1781160684, 0, 'yFrNCQY8xvqU5x8KGBWNC6aQFToTe4BekLM7PJTWBMy4xjM8ahvpXD8PJaNBqvAvQKNKg5wfJkag1USdtagoG9a', 425712540),
    (31, '6AjHFvd7yAgz1XS9SVrtAtUwwbBTvq8PsgJgHzVRkY6R', 1781161888612, 1781161933, 0, '3zm3WvBpXoQsanzY6sJyUXMDmLx8UR81Q8b67Z8Zi54tfYMXu47nEwRbTi8bKvQd3mdYUDqn6QRxv4sWdixHD1GZ', 425715762),
    (33, 'F3xEzAmZRDwLttXZkypxtsq5aHbVN4ZMmhEJeKV8w333', 1781162015588, 1781162032, 0, '5YUfyrzTk3kf8WB1vgnTcv6xAkb2LkojnysUjzbWjSBqet33kcJxNNRiGMMhCgWB1Qxbk2LpesCf3FFYdEvGLkta', 425715914),
    (35, '2zirnvvFoWoZFH5NXm74wXQiedzc6fqYJY16JuTXPxPn', 1781163466473, 1781163473, 0, '5urfqYw4XBnm695aY6pNgKjuQfXpVgeeYT6tCjucrzkoFicztEckmcn2Wy66Hxvjh9CFTfntYYvirydxG58VkDEC', 425719546),
    (37, '82o6gHz99BynQ3j3ZTHsPYv7FuqmSvxgyFs2wGFWSUVt', 1781249870155, 1781249885, 0, '5C1udQ7JCDxoYdQjMhmnGrfUVE8XZxPhr3pRAiKnvosDEY7MJMDYpAmtuPfXXfckCZJtHk2uq1kkjjN5GaS2o9wC', 425937433),
    (39, 'Ft5g2XzbiMMsvAwv4aEpJVCzcsKjunp1D1YCHHr3mQuU', 1781270481484, 1781270494, 0, '5Lq32LyfSSGEYPhVXoMgLGJ6Ppq7eJdMf469LkvAFeF1gd42dMhMLYKAMHxJgBLa9G9ofDAh5SdTmCz2ux7obD3n', 425989343),
    (41, 'CdNN4qDqBGWmwn8W5jMo1hzrUf5fTQc5CXLZzCadCjU1', 1781271131853, 1781271137, 0, 'vxaBAiuo3bYrVeoVjwutKoVfSEscGwY2k8YvkmcVsXWp962kZMXAdR6uHx4dfH7Ru5NuhQa5xCMqYVok6urJaQr', 425990935),
    (43, '4rt2eJXCnkTUkL48CAm4uViMw4LT65SS4QFtgAJrf6xd', 1781272996858, 1781273002, 0, 'NF9xb4h57aALznPAWgeZvLTrSviFNXyz3NPYiQekVSG8KfrKiSvDAmdHusnFFx4FLUa6H2MeeuQMHTA2bLiahX2', 425995607),
    (45, 'D6TjaiGZdUqV1NZXvzhyNYnzhXcCWcQrNEyg12NF6Vua', 1781273261378, 1781273263, 0, '4W7vB2JJNHGd9mfmGQ9bZFp4UR1kTbV2Z8h9S59shT8iPzJsj1d4svEGDyxseK4myyXHJqTTdBteUQbVHJ1YBZyT', 425996263),
    (51, '5sDfE6Dd1ZhiRLNvDUk7vJ1DTsMp9urjdrzxKxH2vBQi', 1781597542154, 1781597558, 0, '5qWi9Vq4PNPskU13Kcc1VFP4FyoArEGrCK5djC94EFVUouG5C15DXAPPwsAb5Ap6pFaF8RUcHTxiSKm4L4SbAWGa', 426813509),
    (53, 'GirFaJEgKD1ZteUN2fBc6fuXbVwfTHNW5E8ZBbvgN8Yh', 1781600217203, 1781600244, 0, '5mQUNVQtAfZne3k1HRHnsWoBxXNzridihATanD2URaX2r3NMW5Eh1q1fqJV8JYQ7chHJaAJPjo4AnJtmk9NxgLvz', 426820311),
    (55, 'AHEVMGvGu1rjuTDKgbnS83jWae7cpLw6Jh5wYGcSTrwD', 1781602381069, 1781602387, 0, '2ah9z64PuXsGmhsu2sDMYhhLQm19YzA3EZ1n8hNYBnnoHYKgKV44ZU6TojMGNztzW98US8RXu24fVXKhEu79FsYB', 426825745),
    (57, 'EjeogoKuHBG3T1QYuthaT6hNXfBKMa8wVjD6vVXj32g1', 1781602519277, 1781602526, 0, 'KeWRCNxFXxCDB1rBTYEQaJB448TdzSbpdHVUZGLxR5mEVgMDbeXknC9fSeuV4oFYjQthhvwyuAi77VKnme4N47e', 426826102),
    (59, 'DjRTGRn3SkCwFxAWGAbwJpbFcn1xdMrjZcN684m5mKFf', 1781602571709, 1781602593, 0, '3JNtRY9KkF9CTz9mYNxwgdTVY418nTAwLj7x2nxBx2ogYPe5pn5DhAgrkRFfF9z1HEUz5soUfX2kMyGs24tHEz98', 426826275),
    (61, '7XXfWciG1WTMRHoFKXcduqGRVvf5Anrv6SXSBDPhPUfo', 1781657832268, 1781657840, 0, '5UH2zJZf6iHF1rJhf6pxcio74CRL9sUT94c3izBgZ82tY9agGhUs3Br2qo4MkcwGrZRcpU9EeG1dFQuamLJtiojK', 426965417),
    (63, 'djmawNBCg3tTzmck8pQTMbxcZmUU6GKppBVJL7Va7Dy', 1781658716182, 1781658737, 0, '55R2fJHVTsdojAeJ1C41p4bSW34pN8RuogVcQjUjfQUzijLXnZaNArAwZDT5KtqAiEmEFNqKcmW1BdfnrfxCK7Uy', 426967787),
    (66, 'D5ZeCs14LxvrjuNEB8MraLDCR1obzRxcbyp8P7HoinAb', 1781681977121, 1781681986, 0, '3PAoxxYwazdphUHsyePr8ycCFThvcA24Wb26NSrRC5JKV8znCrSUKt85jMe9PuHzr3hEH7N8Wx46iXuxmg1RtBwm', 427026294),
    (68, 'F2sxHRRJsxB6tidLWyuwsJGdxYWqtxCPvWoPX8yN163r', 1781687736616, 1781687756, 0, 'XxWnCXrfZZwLq75WL6UUDCp56BPMQShAxFJZpLmCH4AMhmLb6qSqUWipNdTb5ahWHszATnNaepHa3AnaPPxQgfg', 427040875),
    (71, '5FHDsiRN6YEmebMpgtHnGomVYBKZkqRbREr44SNnbkTU', 1781761245855, 1781761272, 0, '38yRKjhcKYsMbtpx3yth6tF7c4ECEZtx8rKsq7xAuDsAF74sw2yNVarmge38MQiFAjQ8zR89gBBGbv6tTQcMhQCh', 427226311),
    (73, 'JDvohojFMbwTkyTD3e9H4xAMVGCTg9pts7YFEFVxdBwe', 1781761445328, 1781761703, 0, '3xd4wiWRh4nX41jwGZQY3huUE4sT8kxAFBh7JWU3pg1BGVvTfzeRdMtycXtFbQMESKMcwhBk7CoXY52d6tsMA9bK', 427227430),
    (75, 'HnBW21wHc1bRbEAy9xEf3hWVySov67oeU8vnEnd7Rmuy', 1781762296267, 1781762366, 0, 'FZU4hjjRFS52xHsK691NAHuCpGnrMGH31VFfSnrfW7T8JQnDaC6H5mNCm1Ur5eFfwuqj46GvqYVtdnQy2sMSkL3', 427229311),
    (77, 'CCxw3ZhQk3VCXm3PUsjFMCnnPgeJA1VHxK1U49HwLtZi', 1781763032441, 1781763042, 0, '67meB5ujWq7VC7kAaK48cEtyhL45vGvYbSFXmFnHZKzf8VZVmyQ3yY1ybUinTNQ8U2r5yWKkFhGvtFp7sYGbJxK', 427230802),
    (79, '58EpXT9XPJevK4H5KCQogXTbfz33tzX4RVuzMxDFBtmQ', 1781765610310, 1781765616, 0, 'zEjD5a8v13BsBS36LK524gEh2VvtgHu8JwZYcUdTmMjvfmegf2hZgcnFY4pvtwq8x8kk8BoGccxf6r1VN2uyeqF', 427237310),
    (81, 'HxSoEEEYFiEFr6PW2TuZcEqxbch1TMqUoRNxeg7Kvh2u', 1781768863401, 1781768896, 0, '4GU9UasfmCR1dt9XGTabbMpF1mCiNYsNTf8GhLMMkaBN1kFs9uc6v1Ft794HTtHzL9toCofCh6Ve5JnWXiDQD4t6', 427245582),
    (83, 'AaB9UPaxbpbCyAkq1688UbdcVHRrRFqHxwFDfLGDu1v6', 1781769354161, 1781769364, 0, '3uisxQzk21Wgm32uBNEJu4yLMUhB26pcjzBvCRrr16BqKT1sd3xEvhAhwDiwoDAutaH7LyWUVr1X61LeE8zVMdvo', 427246760),
    (85, 'XQQmGHCn9e8RW6CTM2gUnNvWRT8it7sKDxE3YJTXwx5', 1781772261952, 1781772289, 0, '4khn6SfYKcU76ft6PgL7EVQVCFYAHGJRAkiiC1kUMtcHewctVoMUM9xc2uxUMbteqb7vT5QrWLjC6aVWUF9c2oUV', 427254135),
    (91, '6ER2Bvan5pNrAqzLvKgS9ajs72BERwXy7ufpFwCiXhBd', 1781776943173, 1781776950, 0, '2dcroQyqk8WPohejgjANF3gfrKzsxncLfnBiyNaYL17g8wEwykaq3w9pDQdvAsJ2TPZmZkPgU6NsVu6YHmuoPCCq', 427265883),
    (93, 'GGuwXoiHPyVB8J2Qe772UWs8j7BKeXEGatXvGZL4X1ds', 1781778952815, 1781778965, 0, '3L7r7miji9MK7bFQx8Vd2zdnW7P4gFgfrWwBQCJ7TNNaNmSseKv4KAxRokGhq12aLJfuZjKGXnUcR8G7gQJe9SY9', 427271163),
    (96, 'GzSxCnLe3RAxRRP6RMsSUntarQNhVtLsL6rMbuAbp4wX', 1781806520374, 1781806526, 0, '3vFQgvezBUB82WMmUXC8RVvMxViEwPC3R9Mw19vjEiMGAL4Um8aAPPmHZcu4yZVcpbL7yaJLmaPJwmY257XxXVSD', 427340456),
    (98, 'GmDAic1FPoRKhoYhF6T3SDwposnNyko1WidALpkdcBDT', 1781834354366, 1781834364, 0, 'VuVVw1qSXjcqZ4qRHynCGsjmWDb5BS4AaBT64s2Xw8b32iugpAGRZcmjMwY78TdCuSgqZ48hbgddVH12x3fSpWC', 427410777),
    (100, '4T1YiDA8E7VzZgXAqrHcJaM6qvMBbapttFAaH7MFdXdp', 1781854936616, 1781854948, 0, '3KdND7MpMmZWNXue99C1zZ22grJ8MjnThP746E4bJ4uLJu3qgTDHSpkAk4c98VqDKHwi9dWqPCFmDfbbXQqKCDfd', 427462655),
    (102, 'AMWB1yVXskhZ8gRxgcPH1E2YQoNZQgTSApLpUS11zAnS', 1781886653872, 1781886660, 0, '4ZVuYtTwoMkLRtEDx68owZ3LLmkbSDdFxv157igLa94qEbr4NjihRDeMCWAJhQ6mD7778jhXTvF9ZLXfyoY4sqiY', 427542604),
    (104, '39Vwx4KebHEygTc4uU5prQapgLHeGUZgeUcoheGeqJ9p', 1781887286314, 1781887291, 0, '3hN4UPXscLAx4mGmvANNyk7jmyq5DbPjZyKMVbiPR3854WsmigWsf6p4R22smpZtttr12C5ZFjyeGAd7yavpiBDs', 427544199),
    (106, 'AvUA85xXvcBcqizEgxqGpUGCrr1nMPZuJeZWJGUdqcYP', 1782109512274, 1782109526, 0, '2bZ6jepsFXzavYyCKsueqBdqQoSwB4QEkQwC6JFwA7qKwSG2QHD1Csk3XubfdbhfcPhtaGRpnqeN4ZXwa7gZ7Rnh', 428103628),
    (109, 'FvPDySxb7WGvbckUN7FcKEcB4m8PuiXBquPJGejWz876', 1782113222859, 1782113232, 0, 'Wuxp6f6NYWJTtJ2ZppcvC63BQizoAZHV1HnQdvoaSKyBKG81ygcZ7EC1tA5C9Q1enamXoHnp7Z8S1Qs7Z8zT9gR', 428112950),
    (111, '3HTxcpVVxKNb8VKRukexW3DSgaQH9F1jWcEqQpHa5CLq', 1782122619420, 1782122625, 0, '3SSCAqTZFw6fRhvWCh5FUxCT9PP5bE6DZxXb42rJJDHj3TSGGMq1K5oPH4Jd8UWzYbLWU3p4txxX5uNfwX4oPXzp', 428136602),
    (113, '8c8YarHQo8Ppqwu2ReCxKi49eJ2VFdd9dUxFCGeDMUDm', 1782196445170, 1782196449, 0, 'XWVJPmZ2huMPgFNEaxhgRvQ3HRQ4qrAk2JMsSbWbT66BUWvqtuaU8H1ixP6p5h5m4q8dRo6hf9gDBhrMgKBmakc', 428321697),
    (115, '6ntjdhFx4U1Ep9ugF9drbxRoq6S63X5SZgTj39yfd2X3', 1782199870010, 1782199873, 0, '4dXAmLMNTMAzT9EW1tnNAmYbemEANwevjzGHERJ2HhkV9L5zqKxHiPx5KKmfDKrwbUn97SSutWhcrBrJ78YieeBR', 428330278),
    (117, 'FeLjoFwZXkZ3wAMECizpSzrMQWFdwBhuw6CSBTwYRbCa', 1782200118394, 1782200121, 0, '2EoD3i9zhckQihcSPnaJJiTwuif1bfuRjdGyJBtwFtok2Enh5SEeLWoC8TEca4fjhBLoZV5H1JxUjmSEwnhvFhWV', 428330892),
    (121, 'CuRJqYLBFU4aBSK94NTjwAHRvZp2b4m49oqPxSqSYunC', 1782392565536, 1782392585, 0, '3gT1WPVjF1HyoQWEtt5ezuaLnzKSx8RjNGbkMdguTaZcAgq24fEMAYq24pKYndrnNM1gXxYrxhdnb7MR4JLoSvn2', 428811386),
    (124, '9XF6aBXZv85T2nEfs8D8frqRDccZvcicdUdeNVtjH9XH', 1782403103344, 1782403108, 0, '5kwWbJSsCr9u7MhURS5opHGTagjiMddTE8CuccjA2tohp2MW67vWeFviasSoqjejtBPYRgvezD3nfvvYoqAjjuqC', 428837565),
    (126, 'D1dTcJvvQG4koTF49Eh7zx936GhrrRm6TBGZHYB8kuQW', 1782412353392, 1782412359, 0, '4AJhHhbF3gmru237z5ZvJ31B4Bg2ZWirbGguJgx72vrqEg6vE8oiZGpmtLgVE6KYeyAWSzEyC4CxxhqsZ8WrrDWt', 428860565)
)
UPDATE loyal_yield.balance_sweep_targets AS target
SET recurring_delegation_nonce = observed.nonce,
    start_timestamp = COALESCE(target.start_timestamp, observed.start_timestamp),
    recurring_delegation_expiry_timestamp = COALESCE(target.recurring_delegation_expiry_timestamp, observed.expiry_timestamp),
    recurring_delegation_signature = COALESCE(target.recurring_delegation_signature, observed.signature),
    recurring_delegation_confirmed_slot = COALESCE(target.recurring_delegation_confirmed_slot, observed.slot)
FROM observed
WHERE target.id = observed.id
  AND target.recurring_delegation = observed.recurring_delegation
  AND target.recurring_delegation_nonce IS NULL;

-- Target 119's delegation address has no transaction on chain: the
-- delegation was never created, so the target has none.
UPDATE loyal_yield.balance_sweep_targets
SET recurring_delegation = NULL,
    recurring_delegation_nonce = NULL,
    recurring_delegation_expiry_timestamp = NULL,
    recurring_delegation_signature = NULL,
    recurring_delegation_confirmed_slot = NULL,
    start_timestamp = NULL,
    period_length_seconds = NULL
WHERE id = 119
  AND recurring_delegation = 'AbdBsHT9vJFcQaRc9jBud8jucKm4h3om16Rut59bG1Vc';

-- Fails if any target still holds a delegation without its nonce.
ALTER TABLE loyal_yield.balance_sweep_targets
    ADD CONSTRAINT balance_sweep_targets_delegation_nonce_check
        CHECK (recurring_delegation IS NULL OR recurring_delegation_nonce IS NOT NULL),
    ADD CONSTRAINT balance_sweep_targets_delegation_nonce_range_check
        CHECK (recurring_delegation_nonce >= 0);

-- The observer records every confirmed create_recurring, not only those whose
-- delegatee is already a managed vault, so a delegation that lands before its
-- vault or policy is projected is attached when they are.
COMMENT ON TABLE loyal_yield.recurring_delegation_observations IS
    'Confirmed Subscriptions recurring delegations, recorded by the observer stream whether or not a vault or Autodeposit target exists for them yet.';
