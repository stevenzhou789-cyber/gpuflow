# Run only inside the newly installed local GitLab. The token is never printed.
require 'date'
admin = User.find_by_username!('root')
name = 'gpuflow-local-bootstrap'
admin.personal_access_tokens.where(name: name, revoked: false).find_each(&:revoke!)
token = admin.personal_access_tokens.create!(
  name: name,
  scopes: ['api'],
  expires_at: Date.current + 1
)
File.open('/var/opt/gitlab/gitlab-rails/gpuflow-bootstrap-token',
          File::WRONLY | File::CREAT | File::TRUNC, 0600) do |file|
  file.chmod(0600)
  file.write(token.token)
end
puts 'Local one-day bootstrap credential created (value withheld).'
