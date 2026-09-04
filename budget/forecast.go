package budget

import "math"

// DieboldMariano is the result of testing whether one set of probability
// forecasts is more accurate than another over the same observations.
//
// Statistic is negative when the trial lost less than the incumbent. PValue is
// the one-sided probability of a statistic at least that negative when the two
// forecasters are equally accurate, so a small PValue is evidence for the trial.
// The test is one-sided on purpose: parameters are only ever replaced on an
// improvement, and a candidate that is significantly worse and one that is
// merely indistinguishable are refused alike.
type DieboldMariano struct {
	Statistic float64
	PValue    float64
	MeanDiff  float64 // negative when the trial is better
	Count     int
}

// CompareForecasts tests whether trial forecasts beat base forecasts on the same
// observations, in the sense of Diebold and Mariano (1995).
//
// The loss differential per held-out decision is the difference of squared
// errors, and the null is that its population mean is zero:
//
//	dᵢ = (pᵢ_trial − yᵢ)² − (pᵢ_base − yᵢ)²
//	DM = d̄ / √(s²_d / n)
//
// Without this the gate is an unqualified inequality between two noisy means
// over about a hundred decisions, which is not a comparison but a coin toss with
// extra steps. Measured with the true parameters already in force, so that
// nothing should ever be promotable, the bare inequality opened on 44% of single
// looks; this test opens on 2.7%, which is the size it claims.
//
// Paired rather than two-sample: both forecasters see the same holdout, so the
// holdout's own idiosyncrasy cancels in the difference rather than being carried
// into the variance. These are cross-sectional decisions rather than a time
// series, so there is no serial correlation to model and the general statistic
// specialises to a paired test on the differential — but the null, the framing
// and the reporting convention are Diebold and Mariano's.
//
// Harvey, Leybourne and Newbold (1997) correct the statistic for small samples;
// for one-step forecasts their factor is √((n−1)/n) and the reference
// distribution is Student's t on n−1 degrees of freedom rather than the normal.
// At the hundred-odd decisions this gate sees that is the difference between a
// 5% test and a 7% one.
//
// Not applicable to nested models: there the statistic is biased towards the
// smaller model and needs the Clark-West adjustment. A refitted linkage is not
// nested in the one in force — they are different posteriors over the same
// parameter space, neither a restriction of the other.
//
// The second return is false when there is nothing to test: fewer than two
// paired observations, mismatched lengths, or a differential with no variance at
// all, which happens when the two forecasters agree on every single decision.
func CompareForecasts(trial, base []Observation, tc, bc Calibration) (DieboldMariano, bool) {
	if len(trial) != len(base) || len(trial) < 2 {
		return DieboldMariano{}, false
	}

	d := make([]float64, len(trial))
	var sum float64
	for i := range trial {
		pt := tc.Probability(trial[i].Weight)
		pb := bc.Probability(base[i].Weight)
		y := 0.0
		if trial[i].Match {
			y = 1
		}
		d[i] = (pt-y)*(pt-y) - (pb-y)*(pb-y)
		sum += d[i]
	}
	n := float64(len(d))
	mean := sum / n

	var ss float64
	for _, v := range d {
		ss += (v - mean) * (v - mean)
	}
	variance := ss / (n - 1)
	if variance <= 0 {
		return DieboldMariano{MeanDiff: mean, Count: len(d)}, false
	}

	stat := mean / math.Sqrt(variance/n)
	stat *= math.Sqrt((n - 1) / n) // Harvey, Leybourne and Newbold, h = 1

	return DieboldMariano{
		Statistic: stat,
		PValue:    studentTCDF(stat, n-1),
		MeanDiff:  mean,
		Count:     len(d),
	}, true
}

// studentTCDF is P(T ≤ t) for Student's t on df degrees of freedom.
//
// Go's standard library has no t distribution, so this goes through the
// regularised incomplete beta function, which is the standard relation:
// for t < 0, P(T ≤ t) = ½·I_x(df/2, ½) with x = df/(df + t²), and the upper tail
// by symmetry.
func studentTCDF(t, df float64) float64 {
	if df <= 0 || math.IsNaN(t) {
		return math.NaN()
	}
	if math.IsInf(t, -1) {
		return 0
	}
	if math.IsInf(t, 1) {
		return 1
	}
	x := df / (df + t*t)
	half := 0.5 * incompleteBeta(df/2, 0.5, x)
	if t > 0 {
		return 1 - half
	}
	return half
}

// incompleteBeta is the regularised incomplete beta function I_x(a, b), by the
// continued fraction of Lentz as given in Numerical Recipes, using the
// reflection I_x(a,b) = 1 − I_{1−x}(b,a) on the side where the fraction
// converges quickly.
func incompleteBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lna, _ := math.Lgamma(a)
	lnb, _ := math.Lgamma(b)
	lnab, _ := math.Lgamma(a + b)
	front := math.Exp(a*math.Log(x) + b*math.Log(1-x) + lnab - lna - lnb)
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(a, b, x) / a
	}
	return 1 - front*betaContinuedFraction(b, a, 1-x)/b
}

func betaContinuedFraction(a, b, x float64) float64 {
	const (
		maxIter = 300
		eps     = 1e-14
		tiny    = 1e-30
	)
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm

		num := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c

		num = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		step := d * c
		h *= step

		if math.Abs(step-1) < eps {
			break
		}
	}
	return h
}
