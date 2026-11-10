import http from 'k6/http';
import { check, sleep } from 'k6';
import { url, params, tags, systemTags, isSuccess } from './common.js';

export const options = {
  vus: 10,
  duration: '5m',
  tags,
  systemTags,
  thresholds: {
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  check(http.get(url, params), { success: isSuccess });
  sleep(0.1);
}
