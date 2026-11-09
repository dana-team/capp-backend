import http from 'k6/http';
import { check, sleep } from 'k6';
import { url, params, tags, isSuccess } from './common.js';

export const options = {
  vus: 10,
  duration: '5m',
  tags,
  thresholds: {
    http_req_duration: ['p(99)<500'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  check(http.get(url, params), { success: isSuccess });
  sleep(0.1);
}
