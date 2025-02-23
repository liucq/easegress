/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ratelimiter

import (
	"os"
	"sync"
	"testing"
	"time"
)

var (
	now time.Time
)

func setup() {
	now = time.Now()
	nowFunc = func() time.Time {
		return now
	}
}

func TestMain(m *testing.M) {
	setup()
	code := m.Run()
	os.Exit(code)
}

func TestConcurrent(t *testing.T) {
	policy := Policy{
		LimitRefreshPeriod: time.Millisecond * 10,
		TimeoutDuration:    time.Millisecond * 50,
		LimitForPeriod:     5,
	}

	var wg sync.WaitGroup
	limiter := New(&policy)
	fn := func() {
		permitted := limiter.WaitPermission()
		if !permitted {
			t.Errorf("WaitPermission should succeed")
		}
		wg.Done()
	}

	wg.Add(30)
	for i := 0; i < 30; i++ {
		go fn()
	}
	wg.Wait()

	if limiter.WaitPermission() {
		t.Errorf("WaitPermission should fail")
	}

	now = now.Add(time.Millisecond * 5)
	if limiter.WaitPermission() {
		t.Errorf("WaitPermission should fail")
	}

	now = now.Add(time.Millisecond * 5)
	wg.Add(5)
	for i := 0; i < 5; i++ {
		go fn()
	}
	wg.Wait()

	if limiter.WaitPermission() {
		t.Errorf("WaitPermission should fail")
	}

	now = now.Add(time.Millisecond * 100)
	wg.Add(30)
	for i := 0; i < 30; i++ {
		go fn()
	}
	wg.Wait()

	if limiter.WaitPermission() {
		t.Errorf("WaitPermission should fail")
	}
}

func TestRateLimiter(t *testing.T) {
	policy := Policy{
		LimitRefreshPeriod: time.Millisecond * 10,
		TimeoutDuration:    time.Millisecond * 50,
		LimitForPeriod:     5,
	}

	limiter := New(&policy)
	for i := 0; i < 30; i++ {
		permitted, d := limiter.AcquirePermission()
		if !permitted {
			t.Errorf("AcquirePermission should succeed: %d", i)
		}
		if d != time.Duration(i/policy.LimitForPeriod)*policy.LimitRefreshPeriod {
			t.Errorf("wait duration of %d should not be: %s", i, d.String())
		}
	}

	if permitted, d := limiter.AcquirePermission(); permitted {
		t.Errorf("AcquirePermission should fail")
	} else if d != policy.TimeoutDuration {
		t.Errorf("wait duration should not be: %s", d.String())
	}

	now = now.Add(time.Millisecond * 5)
	if permitted, d := limiter.AcquirePermission(); permitted {
		t.Errorf("AcquirePermission should fail")
	} else if d != policy.TimeoutDuration {
		t.Errorf("wait duration should not be: %s", d.String())
	}

	now = now.Add(time.Millisecond * 6)
	for i := 0; i < 5; i++ {
		if permitted, d := limiter.AcquirePermission(); !permitted {
			t.Errorf("AcquirePermission should succeed: %d", i)
		} else if d != policy.TimeoutDuration-time.Millisecond {
			t.Errorf("wait duration of %d should not be: %s", i, d.String())
		}
	}

	if permitted, d := limiter.AcquirePermission(); permitted {
		t.Errorf("AcquirePermission should fail")
	} else if d != policy.TimeoutDuration {
		t.Errorf("wait duration should not be: %s", d.String())
	}

	now = now.Add(time.Millisecond * 89)
	for i := 0; i < 30; i++ {
		if permitted, d := limiter.AcquirePermission(); !permitted {
			t.Errorf("AcquirePermission should succeed: %d", i)
		} else if d != time.Duration(i/policy.LimitForPeriod)*policy.LimitRefreshPeriod {
			t.Errorf("wait duration of %d should not be: %s", i, d.String())
		}
	}

	if permitted, d := limiter.AcquirePermission(); permitted {
		t.Errorf("AcquirePermission should fail")
	} else if d != policy.TimeoutDuration {
		t.Errorf("wait duration should not be: %s", d.String())
	}
}
