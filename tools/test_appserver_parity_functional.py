import unittest
from appserver_parity_functional import codex_install_command_present

class InstallCommandTests(unittest.TestCase):
    def test_npm_official_alias_and_full_command(self):
        for text in ['npm install -g @openai/codex','```bash\nnpm i -g @openai/codex\n```','npm install --global @openai/codex']:
            self.assertTrue(codex_install_command_present(text))
    def test_other_packages_and_missing_global_flag_do_not_pass(self):
        for text in ['npm i @openai/codex','npm install -g other-package','npm list -g @openai/codex']:
            self.assertFalse(codex_install_command_present(text))

if __name__=='__main__':unittest.main()
