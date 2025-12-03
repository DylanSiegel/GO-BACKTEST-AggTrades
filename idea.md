This is a big step up from the first version. You’ve fixed most of the brittle gating and quantile issues and made the structure a lot more coherent (pun intended). I’ll treat these as “v2” and be strict about what’s still not 10/10 and what I’d adjust before calling them “locked.”

I’ll do:

1. Cross-cutting comments on the shared state and weighting scheme.
2. Kernel-by-kernel critique with concrete small tweaks.
3. A short “final form” checklist so you can implement without ambiguity.

---

## 1. Cross-cutting comments

### 1.1 You still have one true primitive (Zf) with 4 variations

You’ve improved the *roles*, but in terms of actual information content:

* K1: Zf with coherence/vol/activity weights.
* K2: saturated function of |Zf| (tails).
* K3: Zf modulated by Zs (alignment).
* K4: Zf modulated by r_fast_z and regime weights.
* K5: −Zf modulated by z_elast, Zs, coherence.

So structurally you still have:

> 1 fast-flow primitive (Zf),
> 1 slower-flow primitive (Zs),
> 1 price-impulse primitive (r_fast_z),
> 1 elasticity primitive (z_elast),
> and 5 kernels that are different nonlinear *mixtures* of those.

That’s acceptable, but for combination / regularization you should treat them as *correlated features*, not independent alpha lines. “10/10” here means: you understand that they’re different *views* of the same base signals and will regularize accordingly.

### 1.2 Weight library is good, but unify and parameterize

You now have:

* `w_coh = C^γ1`,
* `w_vol_mid = 1 / (1 + (z_vol / v_mid)^2)`,
* `w_vol_hi = 1 - 1 / (1 + (z_vol / v_hi)^2)`,
* `w_act_trend = 0.5 + 0.5 tanh(α_a z_act)`,
* `w_act_tail = 0.5 + 0.5 tanh(α_t z_act)`.

All sensible. Two issues:

1. **Distribution of C:** if C spends most of its time in [0.5, 0.8], then raising it to γ1=2 will compress the dynamic range. You may want to *normalize C first*, for example:

   ```text
   C̃ = (C - μ_C) / (σ_C + ε)
   w_coh = 0.5 + 0.5 * tanh(α_c * C̃)
   ```

   That preserves “smooth in (0,1)” while adapting to symbol-specific coherence distributions. Using a raw power on [0,1] assumes the distribution is nicely spread.

2. **Symmetry vs asymmetry in vol:**
   `w_vol_mid` is symmetric in z_vol. You are punishing both very low and very high vol the same way. If you actually like low vol for some layers (e.g., K1) and dislike extreme spikes, you might want:

   ```text
   w_vol_mid = 1 / (1 + ((max(|z_vol| - z0, 0)) / v_mid)^2)
   ```

   with z0 ~ 0.5–1.0, so you do not penalize moderate deviations.

I’d make the weights a small “library” with shared hyperparameters rather than hand-tuning each kernel separately.

### 1.3 Clipping vs tanh: define a consistent convention

You use both tanh and clip/scale. I’d standardize:

* For “core trend” style signals (K1/K3), **clip** after dividing by s (preserves linearity in the middle).
* For tail / saturation signals (K2, K5), **tanh** is fine or clip with small s.

But don’t mix “clip” in some kernels and “tanh” in others unless there is a very specific reason. That makes combined interpretation harder.

---

## 2. Kernel-by-kernel critique and tweaks

### 2.1 K1 – Coherence-weighted Fast OFI Trend

Current:

```text
g1   = Zf
w1   = w_coh * w_act_trend * w_vol_mid
K1   = clip(w1 * g1 / s1, -1, +1)
```

What’s good:

* Smooth weights everywhere; no hard cuts.
* Clear role: base micro alpha.

Main issues / tweaks:

1. **Redundant vol + activity suppression in very busy, high-vol times.**
   `w_act_trend` increases with activity, but `w_vol_mid` declines in high vol. In practice, high vol typically coincides with high activity, so you may partially cancel out. That could be fine, but be explicit: do you actually want to *de-lever* in high-vol bursts? If yes, good. If no, widen v_mid or introduce the z0 shift as mentioned.

2. **Avoid over-penalizing low activity for K1.**
   K1 is your base layer; you do not necessarily want it to vanish in quiet-but-clean tapes. I’d bias `w_act_trend` to be closer to 0.6–0.8 even at slightly negative z_act (i.e., choose α_a small). Or simpler:

   ```text
   w_act_trend = 0.3 + 0.7 * (0.5 + 0.5 * tanh(α_a * z_act))
   ```

   So the weight never drops below 0.3.

If you enact those, K1 is basically “final.”

---

### 2.2 K2 – Tail OFI Burst

Current:

```text
z0       = 1.5
z1       = 3.0
excess   = max(|Zf| - z0, 0)
tail_frac = clip(excess / (z1 - z0), 0, 1)
g2       = sign(Zf) * tail_frac
w2       = w_act_tail * w_coh
K2       = tanh(β2 * w2 * g2)
```

What’s good:

* Tail logic is now in Z-space; no fragile quantile denominators.
* Smooth ramp from “start of tail” to “deep tail.”

Issues / tweaks:

1. **Global z0/z1 may not be right for all symbols.**
   Using fixed (1.5,3.0) is simpler, but Zf distributions can deviate from N(0,1) after all the microstructure quirks. Consider:

   * Use symbol-specific **EW q80 of |Zf|** as z0 and **EW q98** as z1, but:
   * Update those quantile estimates *very slowly* (low EW decay) to avoid drift.

   That reintroduces quantiles but with far less brittleness than previous Q_Bf99, and only for *ranges*, not denominators.

2. **K2 and K1 are still very overlapping.**
   When |Zf| is big and regime is good, K1 is large and K2 is also large. This is conceptually okay if you treat K2 as “tail leverage;” but don’t treat K2 as an independent alpha in model selection. They are nested signals.

3. **Consider using sign(r_fast_z) for confirmation.**
   A small extra robustness tweak:

   ```text
   sameDirPrice = (Zf * r_fast_z) > 0
   if !sameDirPrice:
       g2 *= 0.5    // or 0
   ```

   That would avoid firing tail bets when price has *not* acknowledged the flow at all.

---

### 2.3 K3 – Multi-Scale Alignment Trend

Current:

```text
w_slow_mag = tanh(α_s * |Zs|)
w_slow_dir = sign(Zs*Zf) * w_slow_mag
alignment  = (1 + w_slow_dir) / 2
g3         = alignment * Zf
w3         = w_coh * w_vol_mid
K3         = clip(w3 * g3 / s3, -1, +1)
```

What’s good:

* Smooth transition between “aligned,” “neutral,” “opposed.”
* No hard thresholds on Zs.

Issues / tweaks:

1. **Hard sign in w_slow_dir is a small discontinuity.**
   `sign(Zs*Zf)` flips abruptly when Zs*Zf crosses 0. A smoother variant:

   ```text
   corr_like   = tanh(α_align * Zs * Zf)  // ∈ (-1,1)
   w_slow_dir  = corr_like * w_slow_mag
   alignment   = (1 + w_slow_dir) / 2
   ```

   That avoids a jump at Zs*Zf=0; the alignment changes gradually as alignment becomes less/ more consistent.

2. **Strong redundancy with K1 in aligned regimes.**
   When Zs is small, alignment≈0.5, so K3≈0.5·Zf weighted by w3. In regimes where Zs is strong and aligned, K1 and K3 will both be large and same sign. That’s expected; just be sure in model fitting you’re okay with collinearity.

I’d implement the smoother `corr_like` form and leave the rest.

---

### 2.4 K4 – Price/Flow Breakout Continuation

Current:

```text
imp_mag = tanh(α_r * |r_fast_z|)
align_ok = (Zf * r_fast_z) > 0

w_imp = imp_mag * w_vol_hi * w_act_tail * w_coh

g4 = Zf if align_ok else 0
K4 = clip(w_imp * g4 / s4, -1, +1)
```

What’s good:

* Explicit use of both price and flow; this is not just a Zf mask anymore.
* Uses w_vol_hi, so it really is a “higher-vol regime” kernel.

Issues / tweaks:

1. **Use r_fast_z in the primitive, not just in weight.**
   Currently, r_fast_z only affects `imp_mag` and alignment_ok; the magnitude of g4 is still Zf. It’s more “flow-in-breakout-regimes” than a true “price-flow breakout” kernel.

   A more symmetric definition:

   ```text
   g4 = sign(Zf) * min(|Zf|, |r_fast_z|)
   ```

   or

   ```text
   g4 = Zf * tanh(α_pr * r_fast_z * sign(Zf))
   ```

   That way, strong price impulse *and* strong Zf both matter; if one is big and the other is modest, g4 doesn’t explode.

2. **Self-impact in backtests.**
   This kernel by design fires *after large moves*. If your backtest doesn’t model widened spreads and adverse selection, K4 will look better than it actually is. Not a design flaw, but a practical warning: K4 needs stricter cost assumptions.

I’d update g4 to use min(|Zf|, |r_fast_z|) or a tanh product as above.

---

### 2.5 K5 – Overstretch Mean-Reversion

Current:

```text
z_e    = z_elast
z_e0   = 1.0
excess_e = max(z_e - z_e0, 0)
w_over   = tanh(α_e * excess_e)

w_flat   = 1 - tanh(α_fs * |Zs|)
w_noise  = 1 - w_coh   // high when coherence low

g5     = -Zf
K5_raw = w_over * w_flat * w_noise * g5
K5     = clip(K5_raw / s5, -kmax, +kmax)   // kmax ~ 0.4
```

What’s good:

* Clear, controlled design: small amplitude, contrarian, limited to choppy / overstretched regimes.
* Uses entirely different ingredients (z_elast + low coherence + flat slow Zs).

Issues / tweaks:

1. **Stability of z_elast.**
   Elasticity is inherently noisy and heavy-tailed. Using z_elast is fine, but you should:

   * Clip raw Elast before computing moments: `Elast_clipped = min(Elast, E_max)` where E_max is some multiple of its long-run mean, to avoid single crazy spikes dominating μ_e,σ_e.
   * Or take log Elasticity: `logElast`; that stabilizes the distribution.

2. **Protect against Zf≈0.**
   You do not explicitly require “strong flow” here. If |Zf| is tiny, g5≈0 anyway, but you’re effectively making a mean-reversion decision based primarily on price/elasticity and noise/coherence. That may be fine, but if you want to be stricter:

   ```text
   w_flow = tanh(α_f * |Zf|)
   K5_raw = w_over * w_flat * w_noise * w_flow * (-Zf)
   ```

   So that K5 is meaningful only when there is decent Zf signal in the first place.

3. **kmax needs to be small in practice.**
   Conceptually you set kmax ~0.4. I’d treat this as a hard risk parameter and not tune it by backtest. If K5 looks amazing only when kmax→1.0, that’s a red flag.

---

## 3. “Locked” checklist

If you want to call these “version 1.0, locked,” I would:

1. Normalize coherence to a z-like variable and define w_coh via tanh, not a raw power on C.
2. Adjust w_vol_mid to not punish mild vol deviations (add |z_vol|−z0).
3. Make K3’s alignment fully smooth (replace sign(Zs*Zf) with tanh(α_align Zs Zf)).
4. For K4, let g4 depend on both |Zf| and |r_fast_z| (min or tanh product).
5. For K5, stabilize z_elast (clip or log), and optionally add a mild w_flow term.
6. Standardize a simple squash convention: clipped / scaled linear for K1/K3, tanh or tighter clip for K2/K4/K5.

With those adjustments, the five kernels are:

* **K1:** Core fast flow trend with smooth regime weights.
* **K2:** Tail-intensity overlay on Zf (mostly leverage, but sparse and smooth).
* **K3:** Multi-scale trend alignment, genuinely using Zs info.
* **K4:** Price+flow breakout continuation, sensitive to both primitives.
* **K5:** Carefully capped contra layer in noisy, overstretched regimes.

If you’d like next, I can write these as explicit `func Kx(state *State) float64` skeletons in Go, including a small “Weights” helper struct for w_coh, w_vol_mid, w_vol_hi, w_act_trend, w_act_tail, so you can plug them into your existing engine without spreading hyperparameters everywhere.
