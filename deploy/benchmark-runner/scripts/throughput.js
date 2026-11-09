import http from 'k6/http';
import { check } from 'k6';
import { url, params, tags, isSuccess } from './common.js';

export const options = {
  stages: [
    { duration: '1m', target: 10 },
    { duration: '2m', target: 50 },
    { duration: '2m', target: 100 },
    { duration: '1m', target: 0 },
  ],
  tags,
  thresholds: {
    http_req_failed: ['rate<0.05'],
  },
};

export default function () {
  check(http.get(url, params), { success: isSuccess });
}
