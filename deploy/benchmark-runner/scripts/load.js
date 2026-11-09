// Sustained load that drives Knative autoscaling during the scale check.
// scale-test.sh watches the autoscaler react while this runs.
import http from 'k6/http';
import { url, params, tags } from './common.js';

const VUS = parseInt(__ENV.LOAD_VUS, 10);

export const options = {
  stages: [
    { duration: '30s', target: VUS }, // ramp up to push concurrency past the autoscaling target
    { duration: __ENV.LOAD_DURATION, target: VUS }, // hold to let pods scale out
  ],
  tags,
};

export default function () {
  // No think-time: each VU stays in-flight, which is what the concurrency autoscaler scales on.
  http.get(url, params);
}
