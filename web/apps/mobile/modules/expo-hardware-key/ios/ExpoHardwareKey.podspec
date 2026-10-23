Pod::Spec.new do |s|
  s.name           = 'ExpoHardwareKey'
  s.version        = '0.1.0'
  s.summary        = 'Secure Enclave key that wraps and unwraps wallet secrets'
  s.description    = 'A non-exportable P-256 Secure Enclave key; secrets are sealed with ECIES (X9.63 SHA-256, AES-GCM) and opened inside the enclave.'
  s.author         = ''
  s.homepage       = 'https://askloyal.com'
  s.platforms      = {
    :ios => '15.1'
  }
  s.source         = { git: '' }
  s.static_framework = true

  s.dependency 'ExpoModulesCore'

  s.pod_target_xcconfig = {
    'DEFINES_MODULE' => 'YES',
    'SWIFT_COMPILATION_MODE' => 'wholemodule'
  }

  s.source_files = "**/*.{h,m,swift}"
end
