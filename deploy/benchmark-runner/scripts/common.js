// Settings shared by every benchmark. The backend sets these on the Job.
export const url = `${__ENV.TARGET_URL}${__ENV.TARGET_PATH}`;
export const params = { headers: JSON.parse(__ENV.TARGET_HEADERS || '{}') };
export const tags = {
  capp: __ENV.CAPP_NAME,
  namespace: __ENV.CAPP_NAMESPACE,
  cluster: __ENV.CAPP_CLUSTER,
  run_id: __ENV.RUN_ID,
};
// k6's default system tags include url and name, which carry the full request
// URL and so any token in its query string. Metrics keep every other default
// system tag, so only url and name are left out.
export const systemTags = [
  'proto', 'subproto', 'status', 'method', 'group', 'check', 'error', 'error_code',
  'tls_version', 'scenario', 'service', 'expected_response',
];
export const isSuccess = (res) => res.status >= 200 && res.status < 400;
